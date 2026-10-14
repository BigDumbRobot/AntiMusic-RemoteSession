// Package testsignal generates a deterministic "session" whose sample
// value is a function of the absolute timeline position, so any captured
// or delivered sample can be checked for the timeline position it claims.
package testsignal

// Value returns the sample for channel ch at absolute timeline sample t.
// Values lie on the 24-bit grid in [-0.5, 0.5), so they survive float32
// and 24-bit integer storage exactly.
func Value(t int64, ch int) float32 {
	h := splitmix64(uint64(t)<<8 | uint64(ch&0xff))
	k := int32(h>>41) - 1<<22 // [-2^22, 2^22)
	return float32(k) / (1 << 23)
}

// Idle is the value written while the transport is stopped. It is off the
// signal's typical values and constant, so misplacement is detectable.
const Idle float32 = 0.75

func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ x>>30) * 0xbf58476d1ce4e5b9
	x = (x ^ x>>27) * 0x94d049bb133111eb
	return x ^ x>>31
}
