/*
   conflux - Distributed database synchronization library
	Based on the algorithm described in
		"Set Reconciliation with Nearly Optimal	Communication Complexity",
			Yaron Minsky, Ari Trachtenberg, and Richard Zippel, 2004.

   Copyright (C) 2012  Casey Marshall <casey.marshall@gmail.com>

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, version 3.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

// Package testing provides some unit-testing support functions.
package testing

import (
	"fmt"
	"net"
	"time"

	gc "gopkg.in/check.v1"
	log "gopkg.in/hockeypuck/logrus.v0"
	"gopkg.in/tomb.v2"

	"github.com/cmars/conflux"
	"github.com/cmars/conflux/recon"
)

func init() {
	log.SetLevel(log.DebugLevel)
}

var ShortDelay = time.Duration(30 * time.Millisecond)
var LongTimeout = time.Duration(30 * time.Second)

type Cleanup func()

type PtreeFactory func() (recon.PrefixTree, Cleanup, error)

type ReconSuite struct {
	Factory PtreeFactory

	sock string
}

func NewReconSuite(factory PtreeFactory) *ReconSuite {
	return &ReconSuite{
		Factory: factory,
	}
}

func (s *ReconSuite) pollRootConvergence(c *gc.C, peer1, peer2 *recon.Peer, ptree1, ptree2 recon.PrefixTree) error {
	var t tomb.Tomb
	t.Go(func() error {
		defer peer1.Stop()
		defer peer2.Stop()
		var zs1 *conflux.ZSet = conflux.NewZSet()
		var zs2 *conflux.ZSet = conflux.NewZSet()
		timer := time.NewTimer(LongTimeout)
	POLLING:
		for {
			select {
			case r1, ok := <-peer1.RecoverChan:
				if !ok {
					break POLLING
				}
				c.Logf("peer1 recover: %v", r1)
				for _, zp := range r1.RemoteElements {
					c.Assert(zp, gc.NotNil)
					peer1.Insert(zp)
				}
				peer1.ExecCmd(func() error {
					root1, err := ptree1.Root()
					if err != nil {
						return err
					}
					zs1 = conflux.NewZSet(recon.MustElements(root1)...)
					return nil
				})
			case r2, ok := <-peer2.RecoverChan:
				if !ok {
					break POLLING
				}
				c.Logf("peer2 recover: %v", r2)
				for _, zp := range r2.RemoteElements {
					c.Assert(zp, gc.NotNil)
					peer2.Insert(zp)
				}
				peer2.ExecCmd(func() error {
					root2, err := ptree2.Root()
					if err != nil {
						return err
					}
					zs2 = conflux.NewZSet(recon.MustElements(root2)...)
					return nil
				})
			case _ = <-timer.C:
				return fmt.Errorf("timeout waiting for convergence")
			default:
			}
			if zs1.Len() > 0 && zs1.Equal(zs2) {
				c.Logf("peer1 has %q, peer2 has %q", zs1, zs2)
				return nil
			}
		}
		return fmt.Errorf("set reconciliation did not converge")
	})
	return t.Wait()
}

func (s *ReconSuite) pollConvergence(c *gc.C, peer1, peer2 *recon.Peer, peer1Needs, peer2Needs *conflux.ZSet, timeout time.Duration) error {
	var t tomb.Tomb
	t.Go(func() error {
		defer peer1.Stop()
		defer peer2.Stop()
		timer := time.NewTimer(timeout)
	POLLING:
		for {
			select {
			case r1, ok := <-peer1.RecoverChan:
				if !ok {
					break POLLING
				}
				c.Logf("peer1 recover: %v", r1)
				peer1.Insert(r1.RemoteElements...)
				peer1Needs.RemoveSlice(r1.RemoteElements)
			case r2, ok := <-peer2.RecoverChan:
				if !ok {
					break POLLING
				}
				c.Logf("peer2 recover: %v", r2)
				peer2.Insert(r2.RemoteElements...)
				peer2Needs.RemoveSlice(r2.RemoteElements)
			case _ = <-timer.C:
				c.Log("peer1 still needed ", peer1Needs.Len(), ":", peer1Needs)
				c.Log("peer2 still needed ", peer2Needs.Len(), ":", peer2Needs)
				return fmt.Errorf("timeout waiting for convergence")
			default:
			}
			if peer1Needs.Len() == 0 && peer2Needs.Len() == 0 {
				c.Log("all done!")
				return nil
			}
			time.Sleep(ShortDelay)
		}
		return fmt.Errorf("set reconciliation did not converge")
	})
	return t.Wait()
}

func portPair(c *gc.C) (int, int) {
	l1, err := net.Listen("tcp", ":0")
	if err != nil {
		panic(err)
	}
	l2, err := net.Listen("tcp", ":0")
	if err != nil {
		panic(err)
	}
	l1.Close()
	l2.Close()
	return l1.Addr().(*net.TCPAddr).Port, l2.Addr().(*net.TCPAddr).Port
}

func (s *ReconSuite) newPeer(listenPort, partnerPort int, mode recon.PeerMode, ptree recon.PrefixTree) *recon.Peer {
	settings := recon.DefaultSettings()
	settings.ReconAddr = fmt.Sprintf(":%d", listenPort)
	partnerAddr := fmt.Sprintf(":%d", partnerPort)
	settings.Partners[partnerAddr] = recon.Partner{
		ReconAddr: partnerAddr,
	}
	settings.GossipIntervalSecs = 3
	settings.ReadTimeout = 5
	peer := recon.NewPeer(settings, ptree)
	peer.StartMode(mode)
	return peer
}

// Test full node sync.
func (s *ReconSuite) TestFullSync(c *gc.C) {
	ptree1, cleanup, err := s.Factory()
	c.Assert(err, gc.IsNil)
	defer cleanup()

	ptree2, cleanup, err := s.Factory()
	c.Assert(err, gc.IsNil)
	defer cleanup()

	ptree1.Insert(conflux.Zi(conflux.P_SKS, 65537))
	ptree1.Insert(conflux.Zi(conflux.P_SKS, 65539))
	root, _ := ptree1.Root()
	c.Log("peer1:", recon.MustElements(root))

	ptree2.Insert(conflux.Zi(conflux.P_SKS, 65537))
	ptree2.Insert(conflux.Zi(conflux.P_SKS, 65541))
	root, _ = ptree2.Root()
	c.Log("peer2:", recon.MustElements(root))

	port1, port2 := portPair(c)
	peer1 := s.newPeer(port1, port2, recon.PeerModeGossipOnly, ptree1)
	peer2 := s.newPeer(port2, port1, recon.PeerModeServeOnly, ptree2)

	err = s.pollRootConvergence(c, peer1, peer2, ptree1, ptree2)
	c.Assert(err, gc.IsNil)
}

// Test sync with polynomial interpolation.
func (s *ReconSuite) TestPolySyncMBar(c *gc.C) {
	ptree1, cleanup, err := s.Factory()
	c.Assert(err, gc.IsNil)
	defer cleanup()

	ptree2, cleanup, err := s.Factory()
	c.Assert(err, gc.IsNil)
	defer cleanup()

	onlyInPeer1 := conflux.NewZSet()
	// Load up peer 1 with items
	for i := 1; i < 100; i++ {
		ptree1.Insert(conflux.Zi(conflux.P_SKS, 65537*i))
	}
	// Four extra samples
	for i := 1; i < 5; i++ {
		z := conflux.Zi(conflux.P_SKS, 68111*i)
		ptree1.Insert(z)
		onlyInPeer1.Add(z)
	}
	root, _ := ptree1.Root()
	c.Log("peer1:", recon.MustElements(root))

	onlyInPeer2 := conflux.NewZSet()
	// Load up peer 2 with items
	for i := 1; i < 100; i++ {
		ptree2.Insert(conflux.Zi(conflux.P_SKS, 65537*i))
	}
	// One extra sample
	for i := 1; i < 2; i++ {
		z := conflux.Zi(conflux.P_SKS, 70001*i)
		ptree2.Insert(z)
		onlyInPeer2.Add(z)
	}
	root, _ = ptree2.Root()
	c.Log("peer2:", recon.MustElements(root))

	port1, port2 := portPair(c)
	peer1 := s.newPeer(port1, port2, recon.PeerModeGossipOnly, ptree1)
	peer2 := s.newPeer(port2, port1, recon.PeerModeServeOnly, ptree2)

	err = s.pollConvergence(c, peer1, peer2, onlyInPeer2, onlyInPeer1, LongTimeout)
	c.Assert(err, gc.IsNil)
}

// Test sync with polynomial interpolation.
func (s *ReconSuite) TestPolySyncLowMBar(c *gc.C) {
	ptree1, cleanup, err := s.Factory()
	c.Assert(err, gc.IsNil)
	defer cleanup()

	ptree2, cleanup, err := s.Factory()
	c.Assert(err, gc.IsNil)
	defer cleanup()

	onlyInPeer1 := conflux.NewZSet()
	for i := 1; i < 100; i++ {
		ptree1.Insert(conflux.Zi(conflux.P_SKS, 65537*i))
	}
	// extra samples
	for i := 1; i < 50; i++ {
		z := conflux.Zi(conflux.P_SKS, 68111*i)
		onlyInPeer1.Add(z)
		ptree1.Insert(z)
	}
	root1, _ := ptree1.Root()
	c.Log("peer1:", recon.MustElements(root1))

	onlyInPeer2 := conflux.NewZSet()
	for i := 1; i < 100; i++ {
		ptree2.Insert(conflux.Zi(conflux.P_SKS, 65537*i))
	}
	// extra samples
	for i := 1; i < 20; i++ {
		z := conflux.Zi(conflux.P_SKS, 70001*i)
		onlyInPeer2.Add(z)
		ptree2.Insert(z)
	}
	root2, _ := ptree2.Root()
	c.Log("peer2:", recon.MustElements(root2))

	port1, port2 := portPair(c)
	peer1 := s.newPeer(port1, port2, recon.PeerModeGossipOnly, ptree1)
	peer2 := s.newPeer(port2, port1, recon.PeerModeServeOnly, ptree2)

	err = s.pollConvergence(c, peer1, peer2, onlyInPeer2, onlyInPeer1, LongTimeout)
	c.Assert(err, gc.IsNil)
}

func (s *ReconSuite) RunOneSided(c *gc.C, n int, timeout time.Duration) {
	ptree1, cleanup, err := s.Factory()
	c.Assert(err, gc.IsNil)
	defer cleanup()

	ptree2, cleanup, err := s.Factory()
	c.Assert(err, gc.IsNil)
	defer cleanup()

	expected := conflux.NewZSet()

	for i := 1; i < n; i++ {
		z := conflux.Zi(conflux.P_SKS, 65537*i)
		ptree2.Insert(z)
		expected.Add(z)
	}

	port1, port2 := portPair(c)
	peer1 := s.newPeer(port1, port2, recon.PeerModeGossipOnly, ptree1)
	peer2 := s.newPeer(port2, port1, recon.PeerModeServeOnly, ptree2)

	err = s.pollConvergence(c, peer1, peer2, expected, conflux.NewZSet(), timeout)
	c.Assert(err, gc.IsNil)
}