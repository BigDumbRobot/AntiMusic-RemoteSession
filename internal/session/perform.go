package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"antimusic.dev/remotesession/internal/live"
	"antimusic.dev/remotesession/internal/xfer"
)

// The musician's live audio, so the engineer hears the performance as it
// is played (the take itself still travels losslessly as a file).
//
// The client dials a perform connection. The studio answers with one
// JSON line: an error, or an optional UDP offer. The client sends a
// header line (format, rate, channels) and the Opus hello packet, then
// packets: by UDP when offered, and on the TCP connection until the
// studio confirms UDP arrives. The studio sends {"udp_rx":bool} every
// 500 ms; while it says true the client sends only a hello packet every
// 2 s on TCP as keepalive, and when it says false (or stops) TCP carries
// the audio again.

type performOffer struct {
	UDP   *udpOffer `json:"udp,omitempty"`
	Error string    `json:"error,omitempty"`
}

type performAck struct {
	UDPRx bool `json:"udp_rx"`
}

func (h *Host) perform(ctx context.Context, c net.Conn) error {
	if h.Perform == nil {
		fmt.Fprintln(c, `{"error":"the engineer is not taking live audio"}`)
		return nil
	}
	var w *lockedWriter
	var peer *udpPeer
	var offer performOffer
	ready := make(chan struct{})
	if h.UDP != nil {
		var o udpOffer
		peer, o = h.UDP.register(func(p []byte) {
			select {
			case <-ready:
				w.Write(p)
			default: // before the player is open
			}
		})
		defer h.UDP.unregister(peer)
		offer.UDP = &o
	}
	b, _ := json.Marshal(offer)
	c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write(append(b, '\n')); err != nil {
		return err
	}
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, err := br.ReadBytes('\n')
	if err != nil {
		return err
	}
	var hdr streamHeader
	if err := json.Unmarshal(line, &hdr); err != nil || hdr.Format != "opus" {
		return errors.New("live audio: unsupported stream")
	}
	pw, err := h.Perform(StreamFormat{Format: hdr.Format, Rate: hdr.Rate, Channels: hdr.Channels})
	if err != nil {
		return err
	}
	defer pw.Close()
	w = &lockedWriter{w: pw}
	close(ready)
	h.emit(Event{"type": "perform", "state": "playing", "path": "tcp"})

	// Acks, and path changes for the app.
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		path := "tcp"
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-actx.Done():
				return
			case <-t.C:
			}
			rx := peer != nil && peer.audioRecent()
			b, _ := json.Marshal(performAck{UDPRx: rx})
			c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := c.Write(append(b, '\n')); err != nil {
				return
			}
			now := "tcp"
			if rx {
				now = "udp"
			}
			if now != path {
				path = now
				h.emit(Event{"type": "perform", "state": "playing", "path": path})
			}
		}
	}()
	return readPackets(c, br, w)
}

// PacketSource starts the musician's live audio: the hello packet, then
// packets until ctx ends or the channel closes.
type PacketSource func(ctx context.Context) (live.Packet, <-chan []byte, error)

// Perform sends the musician's live audio until ctx ends, reconnecting
// after drops.
func (g *Guest) Perform(ctx context.Context, src PacketSource) error {
	backoff := time.Second
	for ctx.Err() == nil {
		err := g.performOnce(ctx, src)
		if ctx.Err() != nil {
			return nil
		}
		g.emit(Event{"type": "perform", "state": "reconnecting", "error": errString(err)})
		if xfer.IsFatal(err) || !backoffWait(ctx, &backoff) {
			return err
		}
	}
	return nil
}

func (g *Guest) performOnce(ctx context.Context, src PacketSource) error {
	c, err := g.open(ctx, kindPerform)
	if err != nil {
		return err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, err := br.ReadBytes('\n')
	if err != nil {
		return err
	}
	var offer performOffer
	if err := json.Unmarshal(line, &offer); err != nil {
		return err
	}
	if offer.Error != "" {
		return fatalErr{errors.New(offer.Error)}
	}

	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	hello, packets, err := src(sctx)
	if err != nil {
		return fatalErr{err}
	}
	f, ok := live.ParseHello(hello)
	if !ok {
		return errors.New("live audio: bad source format")
	}
	b, _ := json.Marshal(streamHeader{Rate: f.Rate, Channels: f.Channels, Format: "opus"})
	c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write(append(append(b, '\n'), hello...)); err != nil {
		return err
	}

	var u *udpClient
	if offer.UDP != nil && g.UDPHost.IsValid() {
		if u, err = dialUDP(g.UDPHost, *offer.UDP); err == nil {
			defer u.Close()
		} else {
			u = nil
		}
	}
	// Acks from the studio.
	var lastAck, udpOK atomic.Int64 // unix nanos; udpOK: last ack that said true
	readErr := make(chan error, 1)
	go func() {
		for {
			c.SetReadDeadline(time.Now().Add(15 * time.Second))
			line, err := br.ReadBytes('\n')
			if err != nil {
				readErr <- err
				return
			}
			var a performAck
			if json.Unmarshal(line, &a) == nil {
				now := time.Now().UnixNano()
				lastAck.Store(now)
				if a.UDPRx {
					udpOK.Store(now)
				}
			}
		}
	}()
	g.emit(Event{"type": "perform", "state": "sending", "udp": u != nil})
	var lastTCP time.Time
	path := ""
	for {
		var p []byte
		select {
		case err := <-readErr:
			return err
		case p, ok = <-packets:
			if !ok {
				return errors.New("live audio source stopped")
			}
		}
		onUDP := false
		if u != nil {
			u.sendAudio(p)
			ack := time.Unix(0, lastAck.Load())
			onUDP = lastAck.Load() == udpOK.Load() && udpOK.Load() != 0 && time.Since(ack) < udpTimeout
		}
		now := "tcp"
		if onUDP {
			now = "udp"
		}
		if now != path {
			path = now
			g.emit(Event{"type": "perform", "state": "sending", "path": path})
		}
		c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if onUDP {
			if time.Since(lastTCP) > 2*time.Second {
				if _, err := c.Write(hello); err != nil {
					return err
				}
				lastTCP = time.Now()
			}
			continue
		}
		if _, err := c.Write(p); err != nil {
			return err
		}
		lastTCP = time.Now()
	}
}

// fatalErr stops retrying (see xfer.IsFatal).
type fatalErr struct{ error }

func (fatalErr) Fatal() bool     { return true }
func (f fatalErr) Unwrap() error { return f.error }
