package main

import (
	"slices"
	"testing"
	"time"
)

func TestMintArgs(t *testing.T) {
	scopes, exp, err := mintArgs([]string{"items:read", "--expires", "30d", "items:write"})
	if err != nil || !slices.Equal(scopes, []string{"items:read", "items:write"}) || exp == nil {
		t.Fatalf("got %v %v %v", scopes, exp, err)
	}
	if d := time.Until(*exp); d < 30*24*time.Hour-time.Minute || d > 30*24*time.Hour {
		t.Errorf("30d is %v away", d)
	}
	if _, exp, _ := mintArgs([]string{"items:read", "--expires", "12h"}); exp == nil || time.Until(*exp) > 12*time.Hour {
		t.Errorf("12h: %v", exp)
	}
	if _, exp, _ := mintArgs([]string{"items:read"}); exp != nil {
		t.Error("no --expires should not expire")
	}
	for _, bad := range [][]string{{"items:read", "--expires"}, {"items:read", "--expires", "0d"}, {"items:read", "--expires", "soon"}, {"--expires", "1d"}} {
		if _, _, err := mintArgs(bad); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}

func TestInviteArgs(t *testing.T) {
	n, exp, note, err := inviteArgs(nil)
	if err != nil || n != 1 || note != "" {
		t.Fatalf("defaults: %d %q %v", n, note, err)
	}
	if d := time.Until(exp); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour {
		t.Errorf("default expiry is %v away", d)
	}
	n, exp, note, err = inviteArgs([]string{"--count", "10", "--expires", "12h", "--note", "launch night"})
	if err != nil || n != 10 || note != "launch night" || time.Until(exp) > 12*time.Hour {
		t.Fatalf("got %d %v %q %v", n, exp, note, err)
	}
	for _, bad := range [][]string{{"--count"}, {"--count", "0"}, {"--count", "x"}, {"--expires", "0d"}, {"--expires", "soon"}, {"--colour", "red"}, {"10"}} {
		if _, _, _, err := inviteArgs(bad); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}
