package main

import "testing"

func TestParseLikeTheOriginal(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		action string
		flags  map[string]string
		files  int
	}{
		{[]string{"--config-file", "/etc/glance/glance-api.conf", "db_sync"}, "sync", nil, 1},
		{[]string{"--config-file=/a.conf", "--config-file", "/b.conf", "db", "sync"}, "sync", nil, 2},
		{[]string{"db_load_metadefs", "/etc/glance/metadefs", "true"}, "load_metadefs", map[string]string{"path": "/etc/glance/metadefs", "merge": "true"}, 0},
		{[]string{"db", "load_metadefs", "--path", "/m", "--overwrite"}, "load_metadefs", map[string]string{"path": "/m", "overwrite": "true"}, 0},
		{[]string{"db", "purge", "--age_in_days", "7", "--max_rows=-1"}, "purge", map[string]string{"age_in_days": "7", "max_rows": "-1"}, 0},
	} {
		inv, err := parse(tc.args)
		if err != nil || inv.action != tc.action || len(inv.configFiles) != tc.files {
			t.Fatal(tc.args, inv, err)
		}
		for k, v := range tc.flags {
			if inv.flags[k] != v {
				t.Fatal(tc.args, k, inv.flags)
			}
		}
	}
	if _, err := parse([]string{"image-list"}); err == nil {
		t.Fatal("unknown command accepted")
	}
}
