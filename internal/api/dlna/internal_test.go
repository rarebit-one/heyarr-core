package dlna

import "testing"

func TestPageWindow(t *testing.T) {
	in := []int{0, 1, 2, 3, 4}
	if got := page(in, 0, 0); len(got) != 5 {
		t.Errorf("count 0 should mean all, got %d", len(got))
	}
	if got := page(in, 2, 2); len(got) != 2 || got[0] != 2 {
		t.Errorf("start=2 count=2 = %v", got)
	}
	if got := page(in, 10, 0); len(got) != 0 {
		t.Errorf("start past end should be empty, got %v", got)
	}
	if got := page(in, -1, 2); got[0] != 0 {
		t.Errorf("negative start should clamp to 0, got %v", got)
	}
}

func TestClassFor(t *testing.T) {
	cases := map[string]string{
		"movie":   classVideoItem,
		"series":  classVideoItem,
		"music":   classAudioItem,
		"book":    classItem,
		"unknown": classItem,
	}
	for ct, want := range cases {
		if got := classFor(ct); got != want {
			t.Errorf("classFor(%q) = %q, want %q", ct, got, want)
		}
	}
}

func TestFolderTitle(t *testing.T) {
	cases := map[string]string{"movie": "Movies", "series": "TV", "music": "Music", "paper": "Paper", "": "Other"}
	for ct, want := range cases {
		if got := folderTitle(ct); got != want {
			t.Errorf("folderTitle(%q) = %q, want %q", ct, got, want)
		}
	}
}

func TestParseBrowse(t *testing.T) {
	body := []byte(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">` +
		`<ObjectID>ct:movie</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag>` +
		`<StartingIndex>3</StartingIndex><RequestedCount>10</RequestedCount></u:Browse></s:Body></s:Envelope>`)
	req, ok := parseBrowse(body)
	if !ok {
		t.Fatal("a valid Browse did not parse")
	}
	if req.ObjectID != "ct:movie" || req.BrowseFlag != "BrowseDirectChildren" || req.StartingIndex != 3 || req.RequestedCount != 10 {
		t.Fatalf("parsed wrong: %+v", req)
	}
	if _, ok := parseBrowse([]byte(`<nonsense/>`)); ok {
		t.Error("non-Browse body parsed as Browse")
	}
}
