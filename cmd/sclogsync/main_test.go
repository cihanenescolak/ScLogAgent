package main

import (
	"io"
	"testing"
)

func TestParseArgs(t *testing.T) {
	o, err := parseArgs([]string{"--lang", "TR", "uninstall", "--purge"}, io.Discard)
	if err != nil || o.command != "uninstall" || !o.purge || o.lang != "tr" {
		t.Fatalf("%+v %v", o, err)
	}
	o, err = parseArgs([]string{"install-service", "--server", "https://x", "--folder", `C:\A B\LIVE`}, io.Discard)
	if err != nil || o.server != "https://x" || o.folder != `C:\A B\LIVE` {
		t.Fatalf("%+v %v", o, err)
	}
	o, err = parseArgs(nil, io.Discard)
	if err != nil || o.command != "" {
		t.Fatalf("%+v %v", o, err)
	}
	for _, bad := range [][]string{{"bogus"}, {"--lang", "xx"}, {"status", "extra"}, {"--nope"}} {
		if _, err := parseArgs(bad, io.Discard); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
