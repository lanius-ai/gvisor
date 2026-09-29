// Copyright 2024 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package stack

import (
	"context"
	"encoding/binary"
	"math/rand"

	cryptorand "gvisor.dev/gvisor/pkg/rand"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/tcpip"
)

// tunNetworkEndpoints tracks NetworkEndpoints belonging to virtual tun/tap NICs
// whose addresses should be preserved across save/restore.
var tunNetworkEndpoints sync.Map

// beforeSave is invoked by stateify.
func (s *Stack) beforeSave() {
	// removeConf will be set only in case of save/restore.
	s.mu.Lock()
	for _, nic := range s.nics {
		if nic.kind == "tun" {
			for _, netEP := range nic.networkEndpoints {
				tunNetworkEndpoints.Store(netEP, struct{}{})
			}
		}
	}
	if !s.removeConf {
		s.mu.Unlock()
		return
	}

	// Remove all the NICs and routes from the stack as they will be
	// created again during restore based on the new network config,
	// except for virtual tun/tap NICs created inside the sandbox.
	deferActs := make([]func(), 0)
	for id, nic := range s.nics {
		if nic.kind == "tun" {
			continue
		}
		act, _ := s.removeNICLocked(id, true /* closeLinkEndpoint */)
		if act != nil {
			deferActs = append(deferActs, act)
		}
	}
	s.mu.Unlock()

	for _, act := range deferActs {
		act()
	}
}

type savedNICMap = map[tcpip.NICID]*nic
type savedRouteTable = []tcpip.Route

// +checklocksexclude:s.mu
func (s *Stack) saveNics() savedNICMap {
	s.mu.RLock()
	defer s.mu.RUnlock()
	nics := make(map[tcpip.NICID]*nic)
	for id, nic := range s.nics {
		if nic.kind == "tun" {
			nics[id] = nic
		}
	}
	return nics
}

// +checklocksexclude:s.mu
func (s *Stack) loadNics(_ context.Context, nics savedNICMap) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if nics == nil {
		nics = make(map[tcpip.NICID]*nic)
	}
	s.nics = nics
}

// +checklocksexclude:s.mu
// +checklocksexclude:s.routeMu
func (s *Stack) saveRouteTable() savedRouteTable {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.routeMu.RLock()
	defer s.routeMu.RUnlock()
	var routes []tcpip.Route
	for r := s.routeTable.Front(); r != nil; r = r.Next() {
		if nic, ok := s.nics[r.NIC]; ok && nic.kind == "tun" {
			rCopy := *r
			rCopy.RouteEntry = tcpip.RouteEntry{}
			routes = append(routes, rCopy)
		}
	}
	return routes
}

// +checklocksexclude:s.routeMu
func (s *Stack) loadRouteTable(_ context.Context, routes savedRouteTable) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	s.routeTable.Reset()
	for _, r := range routes {
		s.addRouteLocked(&r)
	}
}

func (s *Stack) ensureRNG() {
	if s.insecureRNG == nil {
		var v int64
		if err := binary.Read(cryptorand.Reader, binary.LittleEndian, &v); err != nil {
			panic(err)
		}
		randSrc := &lockedRandomSource{src: rand.NewSource(v)}
		s.insecureRNG = rand.New(randSrc)
	}
	if s.secureRNG.Reader == nil {
		s.secureRNG = cryptorand.RNGFrom(cryptorand.Reader)
	}
}

// afterLoad is invoked by stateify.
func (s *Stack) afterLoad(context.Context) {
	s.ensureRNG()
}

type savedAddressEndpoints = map[tcpip.Address]*addressState
type savedPrimaryAddresses = []*addressState

func (a *AddressableEndpointState) isTunEndpoint() bool {
	if a.networkEndpoint == nil {
		return false
	}
	_, ok := tunNetworkEndpoints.Load(a.networkEndpoint)
	return ok
}

// +checklocksexclude:a.mu
func (a *AddressableEndpointState) saveEndpoints() savedAddressEndpoints {
	if !a.isTunEndpoint() {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.endpoints
}

// +checklocksexclude:a.mu
func (a *AddressableEndpointState) loadEndpoints(_ context.Context, endpoints savedAddressEndpoints) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if endpoints == nil {
		endpoints = make(map[tcpip.Address]*addressState)
	}
	a.endpoints = endpoints
}

// +checklocksexclude:a.mu
func (a *AddressableEndpointState) savePrimary() savedPrimaryAddresses {
	if !a.isTunEndpoint() {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.primary
}

// +checklocksexclude:a.mu
func (a *AddressableEndpointState) loadPrimary(_ context.Context, primary savedPrimaryAddresses) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.primary = primary
}
