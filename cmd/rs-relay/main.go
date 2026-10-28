// Command rs-relay pairs RemoteSession studios and musicians by pairing
// code and passes their encrypted traffic between them. It never sees
// audio or pairing secrets. Run one on any server both sides can reach.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"antimusic.dev/remotesession/internal/pair"
)

func main() {
	listen := flag.String("listen", fmt.Sprintf(":%d", pair.DefaultRelayPort), "address to listen on")
	quiet := flag.Bool("quiet", false, "do not log connections")
	flag.Parse()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	s := &pair.Server{}
	if !*quiet {
		s.Logf = log.Printf
	}
	log.Printf("rs-relay listening on %s", ln.Addr())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := s.Serve(ctx, ln); err != nil {
		log.Fatal(err)
	}
}
