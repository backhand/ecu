package hcloud

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/backhand/ecu/internal/provider"
	hcloudapi "github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// newTestProvider builds a Provider whose hcloud client points at ts with
// instant action polling (ConstantBackoff(0)) so no test ever sleeps. The
// token is irrelevant against the test server; the endpoint override is what
// matters.
func newTestProvider(t *testing.T, ts *httptest.Server, cfg provider.Config) *Provider {
	t.Helper()
	p, err := newWithOptions(cfg,
		hcloudapi.WithToken("test"),
		hcloudapi.WithEndpoint(ts.URL),
		hcloudapi.WithPollOpts(hcloudapi.PollOpts{BackoffFunc: hcloudapi.ConstantBackoff(0)}),
	)
	if err != nil {
		t.Fatalf("newWithOptions: %v", err)
	}
	return p
}

// writeJSON writes a JSON body with the proper content-type (the SDK's error
// path requires a JSON content-type to parse error bodies).
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// --- CreateInstance ----------------------------------------------------------

// TestCreateInstance verifies the POST /servers body carries the user_data,
// server_type, location, image, and the ecu=managed label; that the create
// action (returned already-success so WaitFor is instant) is honored; and that
// the public IP is populated from a follow-up GET /servers/{id}.
func TestCreateInstance(t *testing.T) {
	const serverID = 4711
	var createBody map[string]any

	mux := http.NewServeMux()
	mux.HandleFunc("POST /servers", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &createBody); err != nil {
			t.Errorf("decode create body: %v", err)
		}
		// Return a server with an already-success action so WaitFor returns
		// immediately (no /actions polling). The create response intentionally
		// has an EMPTY public IP to prove CreateInstance re-fetches it.
		writeJSON(w, http.StatusCreated, `{
  "server": {
    "id": 4711, "name": "ecu-test", "status": "initializing",
    "public_net": { "ipv4": { "id": 1, "ip": "", "blocked": false, "dns_ptr": "" },
                    "ipv6": { "id": 2, "ip": "", "blocked": false, "dns_ptr": [] },
                    "floating_ips": [], "firewalls": [] },
    "private_net": [], "server_type": { "id": 22, "name": "cpx21" },
    "datacenter": null, "image": null, "iso": null,
    "protection": { "delete": false, "rebuild": false }, "labels": {}, "volumes": []
  },
  "action": { "id": 1, "command": "create_server", "status": "success",
              "progress": 100, "started": "2024-01-01T00:00:00Z", "finished": "2024-01-01T00:00:05Z",
              "resources": [ { "id": 4711, "type": "server" } ], "error": null },
  "next_actions": [],
  "root_password": null
}`)
	})
	mux.HandleFunc("GET /servers/4711", func(w http.ResponseWriter, r *http.Request) {
		// Now the public IPv4 is assigned.
		writeJSON(w, http.StatusOK, `{
  "server": {
    "id": 4711, "name": "ecu-test", "status": "running",
    "public_net": { "ipv4": { "id": 1, "ip": "203.0.113.42", "blocked": false, "dns_ptr": "" },
                    "ipv6": { "id": 2, "ip": "2001:db8::1", "blocked": false, "dns_ptr": [] },
                    "floating_ips": [], "firewalls": [] },
    "private_net": [], "server_type": { "id": 22, "name": "cpx21" },
    "datacenter": null, "image": null, "iso": null,
    "protection": { "delete": false, "rebuild": false }, "labels": {}, "volumes": []
  }
}`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := newTestProvider(t, ts, provider.Config{DefaultType: "cpx21", DefaultRegion: "hel1"})

	inst, err := p.CreateInstance(t.Context(), provider.InstanceSpec{
		Name:      "ecu-test",
		Type:      "cpx21",
		Region:    "hel1",
		BaseImage: "ubuntu-24.04",
		UserData:  "#cloud-config\n# hi",
		Labels:    map[string]string{"ecu-session": "s_abc"},
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if inst.ID != "4711" {
		t.Fatalf("instance ID = %q, want 4711", inst.ID)
	}
	if inst.PublicIP != "203.0.113.42" {
		t.Fatalf("public IP = %q, want 203.0.113.42 (must be re-fetched via GET)", inst.PublicIP)
	}
	if inst.Status != "running" {
		t.Fatalf("status = %q, want running", inst.Status)
	}

	// Assert the create body.
	if createBody["user_data"] != "#cloud-config\n# hi" {
		t.Fatalf("user_data = %v, want the cloud-init", createBody["user_data"])
	}
	// server_type and image marshal as bare strings (schema.IDOrName) when only
	// a name is set.
	if createBody["server_type"] != "cpx21" {
		t.Fatalf("server_type = %v, want cpx21", createBody["server_type"])
	}
	if createBody["location"] != "hel1" {
		t.Fatalf("location = %v, want hel1", createBody["location"])
	}
	if createBody["image"] != "ubuntu-24.04" {
		t.Fatalf("image = %v, want ubuntu-24.04", createBody["image"])
	}
	labels, _ := createBody["labels"].(map[string]any)
	if labels["ecu"] != "managed" {
		t.Fatalf("labels missing ecu=managed: %v", labels)
	}
	if labels["ecu-session"] != "s_abc" {
		t.Fatalf("labels missing spec label ecu-session: %v", labels)
	}
}

// TestCreateInstanceBootsFromSnapshotID verifies the C7 footgun fix: an
// all-digit BaseImage is sent as an image ID (a snapshot is nameless and booted
// by numeric id), whereas a non-numeric BaseImage is sent as a name. The hcloud
// schema marshals an id-only IDOrName as a JSON NUMBER and a name-only one as a
// STRING, which is exactly how we distinguish the two in the request body.
func TestCreateInstanceBootsFromSnapshotID(t *testing.T) {
	cases := []struct {
		name      string
		baseImage string
		// wantNumeric is true when the image field must marshal as a JSON number
		// (an ID); false when it must be the string name.
		wantNumeric bool
		wantValue   string // the expected stringified value either way
	}{
		{"numeric base image is an ID", "12345", true, "12345"},
		{"named base image is a name", "ubuntu-24.04", false, "ubuntu-24.04"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var createBody map[string]any
			mux := http.NewServeMux()
			mux.HandleFunc("POST /servers", func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &createBody)
				writeJSON(w, http.StatusCreated, `{
  "server": { "id": 4711, "name": "ecu-test", "status": "running",
    "public_net": { "ipv4": { "id": 1, "ip": "203.0.113.42", "blocked": false, "dns_ptr": "" },
                    "ipv6": { "id": 2, "ip": "", "blocked": false, "dns_ptr": [] },
                    "floating_ips": [], "firewalls": [] },
    "private_net": [], "server_type": { "id": 22, "name": "cpx21" },
    "datacenter": null, "image": null, "iso": null,
    "protection": { "delete": false, "rebuild": false }, "labels": {}, "volumes": [] },
  "action": { "id": 1, "command": "create_server", "status": "success",
              "progress": 100, "started": "2024-01-01T00:00:00Z", "finished": "2024-01-01T00:00:05Z",
              "resources": [ { "id": 4711, "type": "server" } ], "error": null },
  "next_actions": [], "root_password": null
}`)
			})
			mux.HandleFunc("GET /servers/4711", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, `{ "server": { "id": 4711, "name": "ecu-test", "status": "running",
    "public_net": { "ipv4": { "id": 1, "ip": "203.0.113.42", "blocked": false, "dns_ptr": "" },
                    "ipv6": { "id": 2, "ip": "", "blocked": false, "dns_ptr": [] },
                    "floating_ips": [], "firewalls": [] },
    "private_net": [], "server_type": { "id": 22, "name": "cpx21" },
    "datacenter": null, "image": null, "iso": null,
    "protection": { "delete": false, "rebuild": false }, "labels": {}, "volumes": [] } }`)
			})
			ts := httptest.NewServer(mux)
			defer ts.Close()

			p := newTestProvider(t, ts, provider.Config{DefaultType: "cpx21"})
			if _, err := p.CreateInstance(t.Context(), provider.InstanceSpec{
				Name: "ecu-test", Type: "cpx21", BaseImage: tc.baseImage,
				UserData: "#cloud-config\n# x",
			}); err != nil {
				t.Fatalf("CreateInstance: %v", err)
			}

			img := createBody["image"]
			switch v := img.(type) {
			case float64: // JSON numbers decode to float64 — this is the ID path
				if !tc.wantNumeric {
					t.Fatalf("image marshaled as a number (%v) but a NAME was expected", v)
				}
				if got := strconv.FormatInt(int64(v), 10); got != tc.wantValue {
					t.Fatalf("image id = %s, want %s", got, tc.wantValue)
				}
			case string:
				if tc.wantNumeric {
					t.Fatalf("image marshaled as a string (%q) but an ID (number) was expected", v)
				}
				if v != tc.wantValue {
					t.Fatalf("image name = %q, want %q", v, tc.wantValue)
				}
			default:
				t.Fatalf("image field has unexpected type %T (%v)", img, img)
			}
		})
	}
}

// TestImageRefHelper unit-tests the id-vs-name detection directly.
func TestImageRefHelper(t *testing.T) {
	if ref := imageRef("98765"); ref.ID != 98765 || ref.Name != "" {
		t.Fatalf("imageRef(numeric) = %+v, want ID=98765 Name=\"\"", ref)
	}
	if ref := imageRef("ubuntu-24.04"); ref.Name != "ubuntu-24.04" || ref.ID != 0 {
		t.Fatalf("imageRef(name) = %+v, want Name=ubuntu-24.04 ID=0", ref)
	}
}

// --- DeleteInstancesByLabel --------------------------------------------------

// TestDeleteInstancesByLabel verifies the orphan-cleanup primitive lists by the
// label selector and deletes each matching server, returning the count.
func TestDeleteInstancesByLabel(t *testing.T) {
	var gotSelector string
	deleted := map[string]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /servers", func(w http.ResponseWriter, r *http.Request) {
		gotSelector = r.URL.Query().Get("label_selector")
		writeJSON(w, http.StatusOK, `{
  "servers": [
    { "id": 101, "name": "ecu-bake", "status": "running",
      "public_net": { "ipv4": { "id": 1, "ip": "203.0.113.7", "blocked": false, "dns_ptr": "" },
                      "ipv6": { "id": 2, "ip": "", "blocked": false, "dns_ptr": [] }, "floating_ips": [], "firewalls": [] },
      "private_net": [], "server_type": { "id": 22, "name": "cpx21" }, "datacenter": null,
      "image": null, "iso": null, "protection": { "delete": false, "rebuild": false },
      "labels": { "ecu-bake": "1" }, "volumes": [] }
  ],
  "meta": { "pagination": { "page": 1, "per_page": 50, "previous_page": null,
            "next_page": null, "last_page": 1, "total_entries": 1 } }
}`)
	})
	mux.HandleFunc("DELETE /servers/101", func(w http.ResponseWriter, r *http.Request) {
		deleted["101"] = true
		writeJSON(w, http.StatusOK, `{ "action": { "id": 11, "command": "delete_server", "status": "success",
              "progress": 100, "started": "2024-01-01T00:00:00Z", "finished": "2024-01-01T00:00:01Z",
              "resources": [ { "id": 101, "type": "server" } ], "error": null } }`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := newTestProvider(t, ts, provider.Config{})
	n, err := p.DeleteInstancesByLabel(t.Context(), "ecu-bake", "1")
	if err != nil {
		t.Fatalf("DeleteInstancesByLabel: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted count = %d, want 1", n)
	}
	if gotSelector != "ecu-bake=1" {
		t.Fatalf("label_selector = %q, want ecu-bake=1", gotSelector)
	}
	if !deleted["101"] {
		t.Fatalf("server 101 was not deleted")
	}
}

// TestDeleteInstancesByLabelNoneMatch verifies that matching nothing returns
// (0, nil) — the idempotent no-op orphan-cleanup runs on every startup.
func TestDeleteInstancesByLabelNoneMatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /servers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{ "servers": [],
  "meta": { "pagination": { "page": 1, "per_page": 50, "previous_page": null,
            "next_page": null, "last_page": 1, "total_entries": 0 } } }`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := newTestProvider(t, ts, provider.Config{})
	n, err := p.DeleteInstancesByLabel(t.Context(), "ecu-bake", "1")
	if err != nil || n != 0 {
		t.Fatalf("DeleteInstancesByLabel(none) = (%d, %v), want (0, nil)", n, err)
	}
}

// --- DeleteInstance ----------------------------------------------------------

// TestDeleteInstanceSuccess verifies a 204 from DELETE /servers/{id} returns
// nil.
func TestDeleteInstanceSuccess(t *testing.T) {
	var deleted bool
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /servers/4711", func(w http.ResponseWriter, r *http.Request) {
		deleted = true
		// The SDK's Delete parses a ServerDeleteResponse (an action), so return
		// a JSON body rather than an empty 204.
		writeJSON(w, http.StatusOK, `{
  "action": { "id": 11, "command": "delete_server", "status": "success",
              "progress": 100, "started": "2024-01-01T00:00:00Z", "finished": "2024-01-01T00:00:01Z",
              "resources": [ { "id": 4711, "type": "server" } ], "error": null }
}`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := newTestProvider(t, ts, provider.Config{})
	if err := p.DeleteInstance(t.Context(), "4711"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if !deleted {
		t.Fatalf("DELETE /servers/4711 was not called")
	}
}

// TestDeleteInstanceNotFoundIsIdempotent verifies that a 404 not_found is
// treated as success (nil) so teardown can be retried safely.
func TestDeleteInstanceNotFoundIsIdempotent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /servers/4711", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, `{"error":{"code":"not_found","message":"server not found"}}`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := newTestProvider(t, ts, provider.Config{})
	if err := p.DeleteInstance(t.Context(), "4711"); err != nil {
		t.Fatalf("DeleteInstance on 404 not_found must be nil (idempotent), got %v", err)
	}
}

// TestDeleteInstanceBadID verifies a non-numeric id never wedges teardown.
func TestDeleteInstanceBadID(t *testing.T) {
	// No server needed: a bad id short-circuits before any request.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request for a bad id: %s %s", r.Method, r.URL.Path)
	}))
	defer ts.Close()

	p := newTestProvider(t, ts, provider.Config{})
	if err := p.DeleteInstance(t.Context(), "fake-1"); err != nil {
		t.Fatalf("DeleteInstance with a non-numeric id must be nil, got %v", err)
	}
}

// --- FindImage ---------------------------------------------------------------

// TestFindImageFound verifies a non-empty image list yields found=true.
func TestFindImageFound(t *testing.T) {
	var gotLabelSelector string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /images", func(w http.ResponseWriter, r *http.Request) {
		gotLabelSelector = r.URL.Query().Get("label_selector")
		writeJSON(w, http.StatusOK, `{
  "images": [ { "id": 99, "type": "snapshot", "status": "available",
                "name": null, "description": "ecu-baked",
                "disk_size": 25, "created": null, "created_from": null,
                "bound_to": null, "os_flavor": "ubuntu", "os_version": null,
                "architecture": "x86", "rapid_deploy": false,
                "protection": { "delete": false }, "deprecated": null,
                "deleted": null, "labels": { "ecu-image": "ecu-baked" } } ],
  "meta": { "pagination": { "page": 1, "per_page": 50, "previous_page": null,
            "next_page": null, "last_page": 1, "total_entries": 1 } }
}`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := newTestProvider(t, ts, provider.Config{})
	img, found, err := p.FindImage(t.Context(), "ecu-baked")
	if err != nil {
		t.Fatalf("FindImage: %v", err)
	}
	if !found {
		t.Fatalf("found = false, want true")
	}
	if img.ID != "99" || img.Name != "ecu-baked" {
		t.Fatalf("image = %+v, want id 99 name ecu-baked", img)
	}
	if gotLabelSelector != "ecu-image=ecu-baked" {
		t.Fatalf("label_selector = %q, want ecu-image=ecu-baked", gotLabelSelector)
	}
}

// TestFindImageAbsent verifies an empty list yields found=false, no error.
func TestFindImageAbsent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /images", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{
  "images": [],
  "meta": { "pagination": { "page": 1, "per_page": 50, "previous_page": null,
            "next_page": null, "last_page": 1, "total_entries": 0 } }
}`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := newTestProvider(t, ts, provider.Config{})
	_, found, err := p.FindImage(t.Context(), "missing")
	if err != nil {
		t.Fatalf("FindImage(absent) returned error: %v", err)
	}
	if found {
		t.Fatalf("found = true, want false for an absent image")
	}
}

// --- CreateImage -------------------------------------------------------------

// TestCreateImage verifies the create_image action returns an Image, and that
// the request carries the snapshot type, description, and labels.
func TestCreateImage(t *testing.T) {
	var body map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /servers/4711/actions/create_image", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeJSON(w, http.StatusCreated, `{
  "action": { "id": 7, "command": "create_image", "status": "success",
              "progress": 100, "started": "2024-01-01T00:00:00Z", "finished": "2024-01-01T00:00:05Z",
              "resources": [ { "id": 4711, "type": "server" } ], "error": null },
  "image": { "id": 555, "type": "snapshot", "status": "creating",
             "name": null, "description": "ecu-baked", "disk_size": 25,
             "created": null, "created_from": { "id": 4711, "name": "ecu-test" },
             "bound_to": null, "os_flavor": "ubuntu", "os_version": null,
             "architecture": "x86", "rapid_deploy": false,
             "protection": { "delete": false }, "deprecated": null,
             "deleted": null, "labels": { "ecu": "managed", "ecu-image": "ecu-baked" } }
}`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := newTestProvider(t, ts, provider.Config{})
	img, err := p.CreateImage(t.Context(), "4711", "ecu-baked")
	if err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	if img.ID != "555" || img.Name != "ecu-baked" {
		t.Fatalf("image = %+v, want id 555 name ecu-baked", img)
	}
	if body["type"] != "snapshot" {
		t.Fatalf("create_image type = %v, want snapshot", body["type"])
	}
	if body["description"] != "ecu-baked" {
		t.Fatalf("create_image description = %v, want ecu-baked", body["description"])
	}
	labels, _ := body["labels"].(map[string]any)
	if labels["ecu-image"] != "ecu-baked" {
		t.Fatalf("create_image labels missing ecu-image: %v", labels)
	}
}

// --- EnsureFirewall ----------------------------------------------------------

// TestEnsureFirewallCreatesDefault verifies that with no existing firewall,
// EnsureFirewall(nil) POSTs /firewalls with the default-deny-in / allow-out
// rules plus a label-selector resource.
func TestEnsureFirewallCreatesDefault(t *testing.T) {
	var createBody map[string]any
	var listName string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /firewalls", func(w http.ResponseWriter, r *http.Request) {
		listName = r.URL.Query().Get("name")
		writeJSON(w, http.StatusOK, `{
  "firewalls": [],
  "meta": { "pagination": { "page": 1, "per_page": 50, "previous_page": null,
            "next_page": null, "last_page": 1, "total_entries": 0 } }
}`)
	})
	mux.HandleFunc("POST /firewalls", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &createBody)
		writeJSON(w, http.StatusCreated, `{
  "firewall": { "id": 321, "name": "ecu-managed", "labels": { "ecu": "managed" },
                "created": "2024-01-01T00:00:00Z", "rules": [], "applied_to": [] },
  "actions": [ { "id": 9, "command": "set_firewall_rules", "status": "success",
                 "progress": 100, "started": "2024-01-01T00:00:00Z", "finished": "2024-01-01T00:00:01Z",
                 "resources": [ { "id": 321, "type": "firewall" } ], "error": null } ]
}`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := newTestProvider(t, ts, provider.Config{})
	if err := p.EnsureFirewall(t.Context(), nil); err != nil {
		t.Fatalf("EnsureFirewall(nil): %v", err)
	}
	if listName != managedFirewallName {
		t.Fatalf("firewall list name filter = %q, want %q", listName, managedFirewallName)
	}
	if createBody["name"] != managedFirewallName {
		t.Fatalf("create firewall name = %v, want %q", createBody["name"], managedFirewallName)
	}

	// Default rules: only outbound allow rules (no inbound), to 0.0.0.0/0 + ::/0.
	rules, _ := createBody["rules"].([]any)
	if len(rules) == 0 {
		t.Fatalf("default firewall has no rules; want outbound allow rules")
	}
	for _, r := range rules {
		rm, _ := r.(map[string]any)
		if rm["direction"] != "out" {
			t.Fatalf("default firewall has a non-outbound rule %v (want block-all-inbound = no inbound rules)", rm)
		}
		dst, _ := rm["destination_ips"].([]any)
		if !containsStr(dst, "0.0.0.0/0") || !containsStr(dst, "::/0") {
			t.Fatalf("outbound rule destinations = %v, want both 0.0.0.0/0 and ::/0", dst)
		}
	}

	// apply_to label selector resource.
	applyTo, _ := createBody["apply_to"].([]any)
	if len(applyTo) != 1 {
		t.Fatalf("apply_to = %v, want a single label-selector resource", createBody["apply_to"])
	}
	res, _ := applyTo[0].(map[string]any)
	if res["type"] != "label_selector" {
		t.Fatalf("apply_to[0].type = %v, want label_selector", res["type"])
	}
	ls, _ := res["label_selector"].(map[string]any)
	if ls["selector"] != "ecu=managed" {
		t.Fatalf("label selector = %v, want ecu=managed", ls)
	}
}

// TestEnsureFirewallSetsRulesOnExisting verifies that with an existing
// firewall, EnsureFirewall calls set_rules on it (not create).
func TestEnsureFirewallSetsRulesOnExisting(t *testing.T) {
	var setRulesCalled bool
	var setRulesBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /firewalls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{
  "firewalls": [ { "id": 321, "name": "ecu-managed", "labels": { "ecu": "managed" },
                   "created": "2024-01-01T00:00:00Z", "rules": [], "applied_to": [] } ],
  "meta": { "pagination": { "page": 1, "per_page": 50, "previous_page": null,
            "next_page": null, "last_page": 1, "total_entries": 1 } }
}`)
	})
	mux.HandleFunc("POST /firewalls", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("POST /firewalls must NOT be called when a firewall already exists")
		writeJSON(w, http.StatusInternalServerError, `{}`)
	})
	mux.HandleFunc("POST /firewalls/321/actions/set_rules", func(w http.ResponseWriter, r *http.Request) {
		setRulesCalled = true
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &setRulesBody)
		writeJSON(w, http.StatusCreated, `{
  "actions": [ { "id": 10, "command": "set_firewall_rules", "status": "success",
                 "progress": 100, "started": "2024-01-01T00:00:00Z", "finished": "2024-01-01T00:00:01Z",
                 "resources": [ { "id": 321, "type": "firewall" } ], "error": null } ]
}`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := newTestProvider(t, ts, provider.Config{})
	// Pass an explicit inbound rule to confirm rule mapping (direction,
	// protocol, port, source CIDRs) reaches the request body.
	rules := []provider.FirewallRule{
		{Direction: provider.DirectionIn, Protocol: provider.ProtocolTCP, Port: "443", SourceCIDRs: []string{"10.0.0.0/8"}},
	}
	if err := p.EnsureFirewall(t.Context(), rules); err != nil {
		t.Fatalf("EnsureFirewall(existing): %v", err)
	}
	if !setRulesCalled {
		t.Fatalf("set_rules was not called on the existing firewall")
	}
	got, _ := setRulesBody["rules"].([]any)
	if len(got) != 1 {
		t.Fatalf("set_rules rules = %v, want 1", setRulesBody["rules"])
	}
	rm, _ := got[0].(map[string]any)
	if rm["direction"] != "in" || rm["protocol"] != "tcp" || rm["port"] != "443" {
		t.Fatalf("rule mapping wrong: %v", rm)
	}
	src, _ := rm["source_ips"].([]any)
	if !containsStr(src, "10.0.0.0/8") {
		t.Fatalf("source_ips = %v, want 10.0.0.0/8", src)
	}
}

// containsStr reports whether want is among the []any of JSON strings.
func containsStr(list []any, want string) bool {
	for _, v := range list {
		if s, _ := v.(string); s == want || strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}
