//go:build !darwin && !linux

package peerproc

func newPlatform() (Resolver, error) { return nil, ErrUnsupported }
