package main

import "testing"

func TestChooseEntryURL(t *testing.T) {
	const publicURL = "https://example.ts.net/?k=private"
	const lanURL = "http://example.ts.net/?k=private"

	if got := chooseEntryURL(publicURL, lanURL, true); got != publicURL {
		t.Fatalf("funnel-ready entry = %q, want %q", got, publicURL)
	}
	if got := chooseEntryURL(publicURL, lanURL, false); got != lanURL {
		t.Fatalf("tailnet-only entry = %q, want %q", got, lanURL)
	}
}
