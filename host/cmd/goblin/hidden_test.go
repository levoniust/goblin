package main

import "testing"

func TestHiddenUsage(t *testing.T) {
	if cmdHidden([]string{"--low"}) == nil || cmdHidden([]string{"--", ""}) == nil {
		t.Fatal("missing program accepted")
	}
	if err := cmdHidden([]string{"--low", "--", "true"}); err != nil {
		t.Fatal(err)
	}
}
