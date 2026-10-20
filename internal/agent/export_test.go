//go:build linux

package agent

import "github.com/AkaraChen/open-agent-runtime/internal/manifest"

// Route points public ports at an instance's private ports (tests and
// benchmarks). inFlight reports the connections still open on the targets
// it replaced.
func (s *Server) Route(name, id string, private map[string]int, public []manifest.Port) (inFlight func() int64, err error) {
	olds, err := s.route(&instance{Name: name, ID: id, Ports: private, PublicPorts: public})
	return func() int64 {
		var n int64
		for _, t := range olds {
			n += t.active.Load()
		}
		return n
	}, err
}

// Unroute closes the public ports that point at instance id.
func (s *Server) Unroute(name, id string) { s.unroute(name, id) }
