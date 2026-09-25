package ui

import (
	"testing"
	"time"
)

// A latency batch whose result never arrives must not freeze the header.
// v1.16.7 set remoteLatencyFetchBusy and then returned early from the tick
// (update re-check), dropping the Cmd: the marker stayed "offline" from the
// probe taken before the network came up, while the remote worked.
func TestArmRemoteLatencyFetch_ReArmsALostBatch(t *testing.T) {
	h := NewHome()
	h.remoteLatencyRefreshSec = 5
	start := time.Now()

	if !h.armRemoteLatencyFetch(start) {
		t.Fatal("first tick must arm a latency batch")
	}
	if h.armRemoteLatencyFetch(start.Add(time.Second)) {
		t.Fatal("a batch in flight must not be armed twice")
	}
	if h.armRemoteLatencyFetch(start.Add(remoteLatencyBusyStale - time.Second)) {
		t.Fatal("a batch within its budget must not be re-armed")
	}
	if !h.armRemoteLatencyFetch(start.Add(remoteLatencyBusyStale + time.Second)) {
		t.Fatal("a batch that never reported must be re-armed after remoteLatencyBusyStale")
	}
}

func TestArmRemoteLatencyFetch_HonoursRefreshAfterAResult(t *testing.T) {
	h := NewHome()
	h.remoteLatencyRefreshSec = 5
	now := time.Now()

	h.Update(remoteLatenciesFetchedMsg{})
	if h.remoteLatencyFetchBusy {
		t.Fatal("a delivered result must clear the busy flag")
	}
	if h.armRemoteLatencyFetch(now.Add(time.Second)) {
		t.Fatal("must wait refresh seconds after the last result")
	}
	if !h.armRemoteLatencyFetch(now.Add(6 * time.Second)) {
		t.Fatal("must arm once refresh seconds have passed")
	}
}
