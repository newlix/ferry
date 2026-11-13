package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func getQuota(t *testing.T, url string) quotaReply {
	t.Helper()
	resp, err := http.Get(url + "/v1/quota")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/quota status = %d, want 200", resp.StatusCode)
	}
	var out quotaReply
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestQuotaReportsEachKey(t *testing.T) {
	usageStub := newZaiUsageStub(t)
	usageStub.weekly["k2"] = 0.1
	srv, gw := newTestGateway(t, func(c *Config) {
		c.Upstreams.Usage = usageStub.server.URL
	})
	gw.refreshUsage()

	q := getQuota(t, srv.URL)
	if q.RefreshedAt == nil || len(q.Keys) != 2 {
		t.Fatalf("reply = %+v, want refreshedAt and 2 keys", q)
	}
	k2 := q.Keys[1]
	if k2.Name != "key2" || k2.Plan != "GLM Coding Pro" || k2.PlanEnd == nil {
		t.Errorf("key2 = %+v", k2)
	}
	if k2.Weekly == nil || k2.Weekly.Left != 0.1 || k2.Weekly.ResetsAt == nil {
		t.Errorf("key2 weekly = %+v, want left 0.1 with a reset", k2.Weekly)
	}
	if k2.Session == nil || k2.Session.Left != 0.9 {
		t.Errorf("key2 session = %+v, want left 0.9", k2.Session)
	}
}

func TestQuotaBeforeFirstCheckHasNoWindows(t *testing.T) {
	srv, _ := newTestGateway(t, nil)
	q := getQuota(t, srv.URL)
	if q.RefreshedAt != nil || q.Keys[0].Weekly != nil || q.Keys[0].Session != nil {
		t.Errorf("reply before the first refresh = %+v, want no refreshedAt and no windows", q)
	}
}
