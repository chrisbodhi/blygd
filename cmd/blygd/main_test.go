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
