// Copyright 2020 The gVisor Authors.
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
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/faketime"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

const (
	entryTestNetNumber tcpip.NetworkProtocolNumber = math.MaxUint32

	entryTestNICID tcpip.NICID = 1
	entryTestAddr1             = tcpip.Address("\x00\x0a\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01")
	entryTestAddr2             = tcpip.Address("\x00\x0a\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02")

	entryTestLinkAddr1 = tcpip.LinkAddress("\x0a\x00\x00\x00\x00\x01")
	entryTestLinkAddr2 = tcpip.LinkAddress("\x0a\x00\x00\x00\x00\x02")

	// entryTestNetDefaultMTU is the MTU, in bytes, used throughout the tests,
	// except where another value is explicitly used. It is chosen to match the
	// MTU of loopback interfaces on Linux systems.
	entryTestNetDefaultMTU = 65536
)

// runImmediatelyScheduledJobs runs all jobs scheduled to run at the current
// time.
func runImmediatelyScheduledJobs(clock *faketime.ManualClock) {
	clock.Advance(immediateDuration)
}

// eventDiffOpts are the options passed to cmp.Diff to compare entry events.
// The UpdatedAtNanos field is ignored due to a lack of a deterministic method
// to predict the time that an event will be dispatched.
func eventDiffOpts() []cmp.Option {
	return []cmp.Option{
		cmpopts.IgnoreFields(NeighborEntry{}, "UpdatedAtNanos"),
	}
}

// eventDiffOptsWithSort is like eventDiffOpts but also includes an option to
// sort slices of events for cases where ordering must be ignored.
func eventDiffOptsWithSort() []cmp.Option {
	return append(eventDiffOpts(), cmpopts.SortSlices(func(a, b testEntryEventInfo) bool {
		return strings.Compare(string(a.Entry.Addr), string(b.Entry.Addr)) < 0
	}))
}

// The following unit tests exercise every state transition and verify its
// behavior with RFC 4681.
//
// | From       | To         | Cause                                      | Update   | Action     | Event   |
// | ========== | ========== | ========================================== | ======== | ===========| ======= |
// | Unknown    | Unknown    | Confirmation w/ unknown address            |          |            | Added   |
// | Unknown    | Incomplete | Packet queued to unknown address           |          | Send probe | Added   |
// | Unknown    | Stale      | Probe                                      |          |            | Added   |
// | Incomplete | Incomplete | Retransmit timer expired                   |          | Send probe | Changed |
// | Incomplete | Reachable  | Solicited confirmation                     | LinkAddr | Notify     | Changed |
// | Incomplete | Stale      | Unsolicited confirmation                   | LinkAddr | Notify     | Changed |
// | Incomplete | Stale      | Probe                                      | LinkAddr | Notify     | Changed |
// | Incomplete | Failed     | Max probes sent without reply              |          | Notify     | Removed |
// | Reachable  | Reachable  | Confirmation w/ different isRouter flag    | IsRouter |            |         |
// | Reachable  | Stale      | Reachable timer expired                    |          |            | Changed |
// | Reachable  | Stale      | Probe or confirmation w/ different address |          |            | Changed |
// | Stale      | Reachable  | Solicited override confirmation            | LinkAddr |            | Changed |
// | Stale      | Reachable  | Solicited confirmation w/o address         |          | Notify     | Changed |
// | Stale      | Stale      | Override confirmation                      | LinkAddr |            | Changed |
// | Stale      | Stale      | Probe w/ different address                 | LinkAddr |            | Changed |
// | Stale      | Delay      | Packet sent                                |          |            | Changed |
// | Delay      | Reachable  | Upper-layer confirmation                   |          |            | Changed |
// | Delay      | Reachable  | Solicited override confirmation            | LinkAddr |            | Changed |
// | Delay      | Reachable  | Solicited confirmation w/o address         |          | Notify     | Changed |
// | Delay      | Stale      | Probe or confirmation w/ different address |          |            | Changed |
// | Delay      | Probe      | Delay timer expired                        |          | Send probe | Changed |
// | Probe      | Reachable  | Solicited override confirmation            | LinkAddr |            | Changed |
// | Probe      | Reachable  | Solicited confirmation w/ same address     |          | Notify     | Changed |
// | Probe      | Reachable  | Solicited confirmation w/o address         |          | Notify     | Changed |
// | Probe      | Stale      | Probe or confirmation w/ different address |          |            | Changed |
// | Probe      | Probe      | Retransmit timer expired                   |          |            | Changed |
// | Probe      | Failed     | Max probes sent without reply              |          | Notify     | Removed |
// | Failed     | Incomplete | Packet queued                              |          | Send probe | Added   |

type testEntryEventType uint8

const (
	entryTestAdded testEntryEventType = iota
	entryTestChanged
	entryTestRemoved
)

func (t testEntryEventType) String() string {
	switch t {
	case entryTestAdded:
		return "add"
	case entryTestChanged:
		return "change"
	case entryTestRemoved:
		return "remove"
	default:
		return fmt.Sprintf("unknown (%d)", t)
	}
}

// Fields are exported for use with cmp.Diff.
type testEntryEventInfo struct {
	EventType testEntryEventType
	NICID     tcpip.NICID
	Entry     NeighborEntry
}

func (e testEntryEventInfo) String() string {
	return fmt.Sprintf("%s event for NIC #%d, %#v", e.EventType, e.NICID, e.Entry)
}

// testNUDDispatcher implements NUDDispatcher to validate the dispatching of
// events upon certain NUD state machine events.
type testNUDDispatcher struct {
	mu     sync.Mutex
	events []testEntryEventInfo
}

var _ NUDDispatcher = (*testNUDDispatcher)(nil)

func (d *testNUDDispatcher) queueEvent(e testEntryEventInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, e)
}

func (d *testNUDDispatcher) OnNeighborAdded(nicID tcpip.NICID, entry NeighborEntry) {
	d.queueEvent(testEntryEventInfo{
		EventType: entryTestAdded,
		NICID:     nicID,
		Entry:     entry,
	})
}

func (d *testNUDDispatcher) OnNeighborChanged(nicID tcpip.NICID, entry NeighborEntry) {
	d.queueEvent(testEntryEventInfo{
		EventType: entryTestChanged,
		NICID:     nicID,
		Entry:     entry,
	})
}

func (d *testNUDDispatcher) OnNeighborRemoved(nicID tcpip.NICID, entry NeighborEntry) {
	d.queueEvent(testEntryEventInfo{
		EventType: entryTestRemoved,
		NICID:     nicID,
		Entry:     entry,
	})
}

type entryTestLinkResolver struct {
	mu     sync.Mutex
	probes []entryTestProbeInfo
}

var _ LinkAddressResolver = (*entryTestLinkResolver)(nil)

type entryTestProbeInfo struct {
	RemoteAddress     tcpip.Address
	RemoteLinkAddress tcpip.LinkAddress
	LocalAddress      tcpip.Address
}

func (p entryTestProbeInfo) String() string {
	return fmt.Sprintf("probe with RemoteAddress=%q, RemoteLinkAddress=%q, LocalAddress=%q", p.RemoteAddress, p.RemoteLinkAddress, p.LocalAddress)
}

// LinkAddressRequest sends a request for the LinkAddress of addr. Broadcasts
// to the local network if linkAddr is the zero value.
func (r *entryTestLinkResolver) LinkAddressRequest(targetAddr, localAddr tcpip.Address, linkAddr tcpip.LinkAddress) tcpip.Error {
	p := entryTestProbeInfo{
		RemoteAddress:     targetAddr,
		RemoteLinkAddress: linkAddr,
		LocalAddress:      localAddr,
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probes = append(r.probes, p)
	return nil
}

// ResolveStaticAddress attempts to resolve address without sending requests.
// It either resolves the name immediately or returns the empty LinkAddress.
func (r *entryTestLinkResolver) ResolveStaticAddress(addr tcpip.Address) (tcpip.LinkAddress, bool) {
	return "", false
}

// LinkAddressProtocol returns the network protocol of the addresses this
// resolver can resolve.
func (r *entryTestLinkResolver) LinkAddressProtocol() tcpip.NetworkProtocolNumber {
	return entryTestNetNumber
}

func entryTestSetup(c NUDConfigurations) (*neighborEntry, *testNUDDispatcher, *entryTestLinkResolver, *faketime.ManualClock) {
	clock := faketime.NewManualClock()
	disp := testNUDDispatcher{}
	nic := nic{
		LinkEndpoint: nil, // entryTestLinkResolver doesn't use a LinkEndpoint

		id: entryTestNICID,
		stack: &Stack{
			clock:           clock,
			nudDisp:         &disp,
			nudConfigs:      c,
			randomGenerator: rand.New(rand.NewSource(time.Now().UnixNano())),
		},
		stats: makeNICStats(),
	}
	netEP := (&testIPv6Protocol{}).NewEndpoint(&nic, nil)
	nic.networkEndpoints = map[tcpip.NetworkProtocolNumber]NetworkEndpoint{
		header.IPv6ProtocolNumber: netEP,
	}

	var linkRes entryTestLinkResolver
	// Stub out the neighbor cache to verify deletion from the cache.
	l := &linkResolver{
		resolver: &linkRes,
	}
	l.neigh.init(&nic, &linkRes)

	entry := newNeighborEntry(&l.neigh, entryTestAddr1 /* remoteAddr */, l.neigh.state)
	l.neigh.mu.Lock()
	l.neigh.mu.cache[entryTestAddr1] = entry
	l.neigh.mu.Unlock()
	nic.linkAddrResolvers = map[tcpip.NetworkProtocolNumber]*linkResolver{
		header.IPv6ProtocolNumber: l,
	}

	return entry, &disp, &linkRes, clock
}

// TestEntryInitiallyUnknown verifies that the state of a newly created
// neighborEntry is Unknown.
func TestEntryInitiallyUnknown(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	if e.mu.neigh.State != Unknown {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Unknown)
	}
	e.mu.Unlock()

	clock.Advance(c.RetransmitTimer)

	// No probes should have been sent.
	linkRes.mu.Lock()
	diff := cmp.Diff([]entryTestProbeInfo(nil), linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	// No events should have been dispatched.
	nudDisp.mu.Lock()
	if diff := cmp.Diff([]testEntryEventInfo(nil), nudDisp.events); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryUnknownToUnknownWhenConfirmationWithUnknownAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Unknown {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Unknown)
	}
	e.mu.Unlock()

	clock.Advance(time.Hour)

	// No probes should have been sent.
	linkRes.mu.Lock()
	diff := cmp.Diff([]entryTestProbeInfo(nil), linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	// No events should have been dispatched.
	nudDisp.mu.Lock()
	if diff := cmp.Diff([]testEntryEventInfo(nil), nudDisp.events); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryUnknownToIncomplete(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Incomplete {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Incomplete)
	}
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
	}
	{
		nudDisp.mu.Lock()
		diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...)
		nudDisp.mu.Unlock()
		if diff != "" {
			t.Fatalf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
		}
	}
}

func TestEntryUnknownToStale(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handleProbeLocked(entryTestLinkAddr1)
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.mu.Unlock()

	// No probes should have been sent.
	runImmediatelyScheduledJobs(clock)
	linkRes.mu.Lock()
	diff := cmp.Diff([]entryTestProbeInfo(nil), linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryIncompleteToIncompleteDoesNotChangeUpdatedAt(t *testing.T) {
	c := DefaultNUDConfigurations()
	c.MaxMulticastProbes = 3
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Incomplete {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Incomplete)
	}
	updatedAtNanos := e.mu.neigh.UpdatedAtNanos
	e.mu.Unlock()

	clock.Advance(c.RetransmitTimer)

	// UpdatedAt should remain the same during address resolution.
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.probes = nil
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	if got, want := e.mu.neigh.UpdatedAtNanos, updatedAtNanos; got != want {
		t.Errorf("got e.mu.neigh.UpdatedAt = %q, want = %q", got, want)
	}
	e.mu.Unlock()

	clock.Advance(c.RetransmitTimer)

	// UpdatedAt should change after failing address resolution. Timing out after
	// sending the last probe transitions the entry to Failed.
	{
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: tcpip.LinkAddress(""),
				LocalAddress:      entryTestAddr2,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	clock.Advance(c.RetransmitTimer)

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestRemoved,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()

	e.mu.Lock()
	if got, notWant := e.mu.neigh.UpdatedAtNanos, updatedAtNanos; got == notWant {
		t.Errorf("expected e.mu.neigh.UpdatedAt to change, got = %q", got)
	}
	e.mu.Unlock()
}

func TestEntryIncompleteToReachable(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Incomplete {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Incomplete)
	}
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Reachable,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryIncompleteToReachableWithRouterFlag(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Incomplete {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Incomplete)
	}
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  false,
		IsRouter:  true,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	if !e.mu.isRouter {
		t.Errorf("got e.mu.isRouter = %t, want = true", e.mu.isRouter)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Reachable,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryIncompleteToStaleWhenUnsolicitedConfirmation(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Incomplete {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Incomplete)
	}
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryIncompleteToStaleWhenProbe(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Incomplete {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Incomplete)
	}
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleProbeLocked(entryTestLinkAddr1)
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryIncompleteToFailed(t *testing.T) {
	c := DefaultNUDConfigurations()
	c.MaxMulticastProbes = 3
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Incomplete {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Incomplete)
	}
	e.mu.Unlock()

	waitFor := c.RetransmitTimer * time.Duration(c.MaxMulticastProbes)
	clock.Advance(waitFor)

	wantProbes := []entryTestProbeInfo{
		// The Incomplete-to-Incomplete state transition is tested here by
		// verifying that 3 reachability probes were sent.
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestRemoved,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()

	e.mu.Lock()
	if e.mu.neigh.State != Failed {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Failed)
	}
	e.mu.Unlock()
}

type testLocker struct{}

var _ sync.Locker = (*testLocker)(nil)

func (*testLocker) Lock()   {}
func (*testLocker) Unlock() {}

func TestEntryStaysReachableWhenConfirmationWithRouterFlag(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	ipv6EP := e.cache.nic.networkEndpoints[header.IPv6ProtocolNumber].(*testIPv6Endpoint)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  false,
		IsRouter:  true,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	if got, want := e.mu.isRouter, true; got != want {
		t.Errorf("got e.mu.isRouter = %t, want = %t", got, want)
	}

	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	if got, want := e.mu.isRouter, false; got != want {
		t.Errorf("got e.mu.isRouter = %t, want = %t", got, want)
	}
	if ipv6EP.invalidatedRtr != e.mu.neigh.Addr {
		t.Errorf("got ipv6EP.invalidatedRtr = %s, want = %s", ipv6EP.invalidatedRtr, e.mu.neigh.Addr)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Reachable,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()

	e.mu.Lock()
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	e.mu.Unlock()
}

func TestEntryStaysReachableWhenProbeWithSameAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	e.handleProbeLocked(entryTestLinkAddr1)
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	if e.mu.neigh.LinkAddr != entryTestLinkAddr1 {
		t.Errorf("got e.mu.neigh.LinkAddr = %q, want = %q", e.mu.neigh.LinkAddr, entryTestLinkAddr1)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Reachable,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryReachableToStaleWhenTimeout(t *testing.T) {
	c := DefaultNUDConfigurations()
	// Eliminate random factors from ReachableTime computation so the transition
	// from Stale to Reachable will only take BaseReachableTime duration.
	c.MinRandomFactor = 1
	c.MaxRandomFactor = 1

	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	e.mu.Unlock()

	clock.Advance(c.BaseReachableTime)

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Reachable,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()

	e.mu.Lock()
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.mu.Unlock()
}

func TestEntryReachableToStaleWhenProbeWithDifferentAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	e.handleProbeLocked(entryTestLinkAddr2)
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Reachable,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr2,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryReachableToStaleWhenConfirmationWithDifferentAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	e.handleConfirmationLocked(entryTestLinkAddr2, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Reachable,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryReachableToStaleWhenConfirmationWithDifferentAddressAndOverride(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	e.handleConfirmationLocked(entryTestLinkAddr2, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  true,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Reachable,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr2,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryStaysStaleWhenProbeWithSameAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.handleProbeLocked(entryTestLinkAddr1)
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	if e.mu.neigh.LinkAddr != entryTestLinkAddr1 {
		t.Errorf("got e.mu.neigh.LinkAddr = %q, want = %q", e.mu.neigh.LinkAddr, entryTestLinkAddr1)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryStaleToReachableWhenSolicitedOverrideConfirmation(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.handleConfirmationLocked(entryTestLinkAddr2, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  true,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	if e.mu.neigh.LinkAddr != entryTestLinkAddr2 {
		t.Errorf("got e.mu.neigh.LinkAddr = %q, want = %q", e.mu.neigh.LinkAddr, entryTestLinkAddr2)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr2,
				State:    Reachable,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryStaleToReachableWhenSolicitedConfirmationWithoutAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.handleConfirmationLocked("" /* linkAddr */, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	if e.mu.neigh.LinkAddr != entryTestLinkAddr1 {
		t.Errorf("got e.mu.neigh.LinkAddr = %q, want = %q", e.mu.neigh.LinkAddr, entryTestLinkAddr1)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Reachable,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryStaleToStaleWhenOverrideConfirmation(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.handleConfirmationLocked(entryTestLinkAddr2, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  true,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	if e.mu.neigh.LinkAddr != entryTestLinkAddr2 {
		t.Errorf("got e.mu.neigh.LinkAddr = %q, want = %q", e.mu.neigh.LinkAddr, entryTestLinkAddr2)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr2,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryStaleToStaleWhenProbeUpdateAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.handleProbeLocked(entryTestLinkAddr2)
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	if e.mu.neigh.LinkAddr != entryTestLinkAddr2 {
		t.Errorf("got e.mu.neigh.LinkAddr = %q, want = %q", e.mu.neigh.LinkAddr, entryTestLinkAddr2)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr2,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryStaleToDelay(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Delay {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryDelayToReachableWhenUpperLevelConfirmation(t *testing.T) {
	c := DefaultNUDConfigurations()
	// Eliminate random factors from ReachableTime computation so the transition
	// from Stale to Reachable will only take BaseReachableTime duration.
	c.MinRandomFactor = 1
	c.MaxRandomFactor = 1

	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Delay {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Delay)
	}
	e.handleUpperLevelConfirmationLocked()
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	e.mu.Unlock()

	clock.Advance(c.BaseReachableTime)
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Reachable,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryDelayToReachableWhenSolicitedOverrideConfirmation(t *testing.T) {
	c := DefaultNUDConfigurations()
	c.MaxMulticastProbes = 1
	// Eliminate random factors from ReachableTime computation so the transition
	// from Stale to Reachable will only take BaseReachableTime duration.
	c.MinRandomFactor = 1
	c.MaxRandomFactor = 1

	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Delay {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Delay)
	}
	e.handleConfirmationLocked(entryTestLinkAddr2, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  true,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	if e.mu.neigh.LinkAddr != entryTestLinkAddr2 {
		t.Errorf("got e.mu.neigh.LinkAddr = %q, want = %q", e.mu.neigh.LinkAddr, entryTestLinkAddr2)
	}
	e.mu.Unlock()

	clock.Advance(c.BaseReachableTime)
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr2,
				State:    Reachable,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr2,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryDelayToReachableWhenSolicitedConfirmationWithoutAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	c.MaxMulticastProbes = 1
	// Eliminate random factors from ReachableTime computation so the transition
	// from Stale to Reachable will only take BaseReachableTime duration.
	c.MinRandomFactor = 1
	c.MaxRandomFactor = 1

	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Delay {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Delay)
	}
	e.handleConfirmationLocked("" /* linkAddr */, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	if e.mu.neigh.LinkAddr != entryTestLinkAddr1 {
		t.Errorf("got e.mu.neigh.LinkAddr = %q, want = %q", e.mu.neigh.LinkAddr, entryTestLinkAddr1)
	}
	e.mu.Unlock()

	clock.Advance(c.BaseReachableTime)
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Reachable,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryStaysDelayWhenOverrideConfirmationWithSameAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Delay {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Delay)
	}
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  true,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Delay {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Delay)
	}
	if e.mu.neigh.LinkAddr != entryTestLinkAddr1 {
		t.Errorf("got e.mu.neigh.LinkAddr = %q, want = %q", e.mu.neigh.LinkAddr, entryTestLinkAddr1)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryDelayToStaleWhenProbeWithDifferentAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Delay {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Delay)
	}
	e.handleProbeLocked(entryTestLinkAddr2)
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr2,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryDelayToStaleWhenConfirmationWithDifferentAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Delay {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Delay)
	}
	e.handleConfirmationLocked(entryTestLinkAddr2, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  true,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr2,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryDelayToProbe(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	{
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: tcpip.LinkAddress(""),
				LocalAddress:      entryTestAddr2,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.probes = nil
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Delay {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Delay)
	}
	e.mu.Unlock()

	clock.Advance(c.DelayFirstProbeTime)
	{
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: entryTestLinkAddr1,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Probe,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()

	e.mu.Lock()
	if e.mu.neigh.State != Probe {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Probe)
	}
	e.mu.Unlock()
}

func TestEntryProbeToStaleWhenProbeWithDifferentAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	{
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: tcpip.LinkAddress(""),
				LocalAddress:      entryTestAddr2,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.probes = nil
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	clock.Advance(c.DelayFirstProbeTime)
	{
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: entryTestLinkAddr1,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	e.mu.Lock()
	if e.mu.neigh.State != Probe {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Probe)
	}
	e.handleProbeLocked(entryTestLinkAddr2)
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Probe,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr2,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryProbeToStaleWhenConfirmationWithDifferentAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	{
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: tcpip.LinkAddress(""),
				LocalAddress:      entryTestAddr2,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.probes = nil
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	clock.Advance(c.DelayFirstProbeTime)
	{
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: entryTestLinkAddr1,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	e.mu.Lock()
	if e.mu.neigh.State != Probe {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Probe)
	}
	e.handleConfirmationLocked(entryTestLinkAddr2, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  true,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Stale {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Stale)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Probe,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr2,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryStaysProbeWhenOverrideConfirmationWithSameAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	{
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: tcpip.LinkAddress(""),
				LocalAddress:      entryTestAddr2,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.probes = nil
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	clock.Advance(c.DelayFirstProbeTime)
	{
		wantProbes := []entryTestProbeInfo{
			// The second probe is caused by the Delay-to-Probe transition.
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: entryTestLinkAddr1,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	e.mu.Lock()
	if e.mu.neigh.State != Probe {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Probe)
	}
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  true,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Probe {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Probe)
	}
	if got, want := e.mu.neigh.LinkAddr, entryTestLinkAddr1; got != want {
		t.Errorf("got e.mu.neigh.LinkAddr = %q, want = %q", got, want)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Probe,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

// TestEntryUnknownToStaleToProbeToReachable exercises the following scenario:
//   1. Probe is received
//   2. Entry is created in Stale
//   3. Packet is queued on the entry
//   4. Entry transitions to Delay then Probe
//   5. Probe is sent
func TestEntryUnknownToStaleToProbeToReachable(t *testing.T) {
	c := DefaultNUDConfigurations()
	// Eliminate random factors from ReachableTime computation so the transition
	// from Probe to Reachable will only take BaseReachableTime duration.
	c.MinRandomFactor = 1
	c.MaxRandomFactor = 1

	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handleProbeLocked(entryTestLinkAddr1)
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	clock.Advance(c.DelayFirstProbeTime)
	wantProbes := []entryTestProbeInfo{
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: entryTestLinkAddr1,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	if e.mu.neigh.State != Probe {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Probe)
	}
	e.handleConfirmationLocked(entryTestLinkAddr2, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  true,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	if got, want := e.mu.neigh.LinkAddr, entryTestLinkAddr2; got != want {
		t.Errorf("got e.mu.neigh.LinkAddr = %q, want = %q", got, want)
	}
	e.mu.Unlock()

	clock.Advance(c.BaseReachableTime)
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Probe,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr2,
				State:    Reachable,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr2,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryProbeToReachableWhenSolicitedOverrideConfirmation(t *testing.T) {
	c := DefaultNUDConfigurations()
	// Eliminate random factors from ReachableTime computation so the transition
	// from Stale to Reachable will only take BaseReachableTime duration.
	c.MinRandomFactor = 1
	c.MaxRandomFactor = 1

	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	{
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: tcpip.LinkAddress(""),
				LocalAddress:      entryTestAddr2,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.probes = nil
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	clock.Advance(c.DelayFirstProbeTime)
	{
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: entryTestLinkAddr1,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	e.mu.Lock()
	if e.mu.neigh.State != Probe {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Probe)
	}
	e.handleConfirmationLocked(entryTestLinkAddr2, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  true,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	if got, want := e.mu.neigh.LinkAddr, entryTestLinkAddr2; got != want {
		t.Errorf("got e.mu.neigh.LinkAddr = %q, want = %q", got, want)
	}
	e.mu.Unlock()

	clock.Advance(c.BaseReachableTime)
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Probe,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr2,
				State:    Reachable,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr2,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryProbeToReachableWhenSolicitedConfirmationWithSameAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	// Eliminate random factors from ReachableTime computation so the transition
	// from Stale to Reachable will only take BaseReachableTime duration.
	c.MinRandomFactor = 1
	c.MaxRandomFactor = 1

	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	{
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: tcpip.LinkAddress(""),
				LocalAddress:      entryTestAddr2,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.probes = nil
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	clock.Advance(c.DelayFirstProbeTime)
	{
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: entryTestLinkAddr1,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	e.mu.Lock()
	if e.mu.neigh.State != Probe {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Probe)
	}
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	e.mu.Unlock()

	clock.Advance(c.BaseReachableTime)
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Probe,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Reachable,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryProbeToReachableWhenSolicitedConfirmationWithoutAddress(t *testing.T) {
	c := DefaultNUDConfigurations()
	// Eliminate random factors from ReachableTime computation so the transition
	// from Stale to Reachable will only take BaseReachableTime duration.
	c.MinRandomFactor = 1
	c.MaxRandomFactor = 1

	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	{
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: tcpip.LinkAddress(""),
				LocalAddress:      entryTestAddr2,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.probes = nil
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	clock.Advance(c.DelayFirstProbeTime)
	{
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: entryTestLinkAddr1,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	e.mu.Lock()
	if e.mu.neigh.State != Probe {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Probe)
	}
	e.handleConfirmationLocked("" /* linkAddr */, ReachabilityConfirmationFlags{
		Solicited: true,
		Override:  false,
		IsRouter:  false,
	})
	if e.mu.neigh.State != Reachable {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Reachable)
	}
	e.mu.Unlock()

	clock.Advance(c.BaseReachableTime)
	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Probe,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Reachable,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryProbeToFailed(t *testing.T) {
	c := DefaultNUDConfigurations()
	c.MaxMulticastProbes = 3
	c.MaxUnicastProbes = 3
	c.DelayFirstProbeTime = c.RetransmitTimer
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	runImmediatelyScheduledJobs(clock)
	{
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress: entryTestAddr1,
				LocalAddress:  entryTestAddr2,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.probes = nil
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
		}
	}

	e.mu.Lock()
	e.handleConfirmationLocked(entryTestLinkAddr1, ReachabilityConfirmationFlags{
		Solicited: false,
		Override:  false,
		IsRouter:  false,
	})
	e.handlePacketQueuedLocked(entryTestAddr2)
	e.mu.Unlock()

	// Observe each probe sent while in the Probe state.
	for i := uint32(0); i < c.MaxUnicastProbes; i++ {
		clock.Advance(c.RetransmitTimer)
		wantProbes := []entryTestProbeInfo{
			{
				RemoteAddress:     entryTestAddr1,
				RemoteLinkAddress: entryTestLinkAddr1,
			},
		}
		linkRes.mu.Lock()
		diff := cmp.Diff(wantProbes, linkRes.probes)
		linkRes.probes = nil
		linkRes.mu.Unlock()
		if diff != "" {
			t.Fatalf("link address resolver probe #%d mismatch (-want, +got):\n%s", i+1, diff)
		}

		e.mu.Lock()
		if e.mu.neigh.State != Probe {
			t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Probe)
		}
		e.mu.Unlock()
	}

	// Wait for the last probe to expire, causing a transition to Failed.
	clock.Advance(c.RetransmitTimer)
	e.mu.Lock()
	if e.mu.neigh.State != Failed {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Failed)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Stale,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Delay,
			},
		},
		{
			EventType: entryTestChanged,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Probe,
			},
		},
		{
			EventType: entryTestRemoved,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: entryTestLinkAddr1,
				State:    Probe,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}

func TestEntryFailedToIncomplete(t *testing.T) {
	c := DefaultNUDConfigurations()
	c.MaxMulticastProbes = 3
	e, nudDisp, linkRes, clock := entryTestSetup(c)

	// TODO(gvisor.dev/issue/4872): Use helper functions to start entry tests in
	// their expected state.
	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Incomplete {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Incomplete)
	}
	e.mu.Unlock()

	waitFor := c.RetransmitTimer * time.Duration(c.MaxMulticastProbes)
	clock.Advance(waitFor)

	wantProbes := []entryTestProbeInfo{
		// The Incomplete-to-Incomplete state transition is tested here by
		// verifying that 3 reachability probes were sent.
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
		{
			RemoteAddress:     entryTestAddr1,
			RemoteLinkAddress: tcpip.LinkAddress(""),
			LocalAddress:      entryTestAddr2,
		},
	}
	linkRes.mu.Lock()
	diff := cmp.Diff(wantProbes, linkRes.probes)
	linkRes.mu.Unlock()
	if diff != "" {
		t.Fatalf("link address resolver probes mismatch (-want, +got):\n%s", diff)
	}

	e.mu.Lock()
	if e.mu.neigh.State != Failed {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Failed)
	}
	e.mu.Unlock()

	e.mu.Lock()
	e.handlePacketQueuedLocked(entryTestAddr2)
	if e.mu.neigh.State != Incomplete {
		t.Errorf("got e.mu.neigh.State = %q, want = %q", e.mu.neigh.State, Incomplete)
	}
	e.mu.Unlock()

	wantEvents := []testEntryEventInfo{
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestRemoved,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
		{
			EventType: entryTestAdded,
			NICID:     entryTestNICID,
			Entry: NeighborEntry{
				Addr:     entryTestAddr1,
				LinkAddr: tcpip.LinkAddress(""),
				State:    Incomplete,
			},
		},
	}
	nudDisp.mu.Lock()
	if diff := cmp.Diff(wantEvents, nudDisp.events, eventDiffOpts()...); diff != "" {
		t.Errorf("nud dispatcher events mismatch (-want, +got):\n%s", diff)
	}
	nudDisp.mu.Unlock()
}
