// SPDX-License-Identifier: Apache-2.0

package exporter

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("bad test fixture: %v", err)
	}
	return m
}

// Regression: a transport node without node_deployment_state (seen in
// production while a host was being added) used to panic with
// "interface conversion: interface {} is nil, not map[string]interface {}".
func TestTransportNodeStateHandlerToleratesMissingDeploymentState(t *testing.T) {
	status := &Nsxv3Resource{
		kind: TransportNode,
		state: decode(t, `{
			"results": [
				{"transport_node_id": "a", "state": "success",
				 "node_deployment_state": {"state": "success"}},
				{"transport_node_id": "b", "state": "in_progress"},
				{"transport_node_id": "c"},
				{"state": "success"},
				"garbage"
			],
			"cursor": "next-page"
		}`),
	}
	data := &Nsxv3Data{}

	cursor, err := transportNodeStateHandler(data, status)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cursor != "next-page" {
		t.Errorf("cursor = %q, want next-page", cursor)
	}

	want := []Nsxv3TransportNodeData{
		{ID: "a", State: 1, DeploymentState: 1},
		{ID: "b", State: 0, DeploymentState: -5},
		{ID: "c", State: -5, DeploymentState: -5},
	}
	if len(data.TransportNodes) != len(want) {
		t.Fatalf("got %d nodes, want %d: %+v", len(data.TransportNodes), len(want), data.TransportNodes)
	}
	for i := range want {
		if data.TransportNodes[i] != want[i] {
			t.Errorf("node %d = %+v, want %+v", i, data.TransportNodes[i], want[i])
		}
	}
}

// Any handler that still uses chained type assertions must not take the
// process down when NSX-T returns an unexpected shape.
func TestHandleRecoversFromHandlerPanic(t *testing.T) {
	status := &Nsxv3Resource{
		kind: TransportNodes, // transportNodesStateHandler asserts float64 fields directly
		request: &http.Request{
			URL: &url.URL{Path: "/api/v1/transport-nodes/status"},
		},
		state: decode(t, `{"up_count": "not-a-number"}`),
	}

	cursor, err := handle(&Nsxv3Data{}, status)
	if err == nil {
		t.Fatal("expected an error from a panicking handler, got nil")
	}
	if cursor != noCursor {
		t.Errorf("cursor = %q, want empty", cursor)
	}
	for _, frag := range []string{"panicked", "/api/v1/transport-nodes/status", "interface conversion"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error %q should mention %q", err, frag)
		}
	}
}
