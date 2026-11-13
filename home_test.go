package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func getHome(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	return string(body)
}

func TestHomeShowsQuotaAndPlanEnd(t *testing.T) {
	usageStub := newZaiUsageStub(t)
	usageStub.weekly["k2"] = 0.1
	srv, gw := newTestGateway(t, func(c *Config) {
		c.Upstreams.Usage = usageStub.server.URL
	})
	gw.refreshUsage()

	body := getHome(t, srv.URL)
	for _, want := range []string{
		"2 keys · quota checked just now",
		"80% left",
		"10% left",
		`data-color="error"`,
		"90% left",
		"Refills in 1h 59m",
		"Ends Aug 12, 2028",
		"GLM Coding Pro",
		srv.URL + "/v1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// the page links ciderui's console stylesheet, and ferry serves it
func TestHomeServesItsStylesheet(t *testing.T) {
	srv, _ := newTestGateway(t, nil)
	body := getHome(t, srv.URL)
	start := strings.Index(body, `href="/ciderui/`)
	if start < 0 {
		t.Fatalf("page links no ciderui stylesheet:\n%s", body)
	}
	href := body[start+len(`href="`):]
	href = href[:strings.Index(href, `"`)]
	resp, err := http.Get(srv.URL + href)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") {
		t.Fatalf("GET %s = %d %q, want 200 text/css", href, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestHomeBeforeFirstQuotaCheck(t *testing.T) {
	srv, _ := newTestGateway(t, nil)
	body := getHome(t, srv.URL)
	if !strings.Contains(body, "checking quota…") || strings.Contains(body, "% left") {
		t.Errorf("page before the first refresh should say it is checking and show no meters:\n%s", body)
	}
}
