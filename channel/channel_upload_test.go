package channel

import "testing"

func TestEmbedURLFromLink(t *testing.T) {
	cases := []struct {
		host, link, want string
	}{
		// LuluStream's "/e/" watch page frame-busts; only "/embed/" can be iframed.
		{"LuluStream", "https://lulustream.com/AbC123xY.html", "https://lulustream.com/embed/AbC123xY"},
		{"LuluStream", "https://lulustream.com/e/AbC123xY", "https://lulustream.com/embed/AbC123xY"},
		{"LuluStream", "https://luluvdo.com/AbC123xY.html", "https://lulustream.com/embed/AbC123xY"},
		{"DoodStream", "https://dood.to/e/xyz789", "https://dood.to/e/xyz789"},
		{"VOE.sx", "https://voe.sx/abc123", "https://voe.sx/e/abc123"},
		{"LuluStream", "", ""},
	}
	for _, cse := range cases {
		if got := embedURLFromLink(cse.host, cse.link); got != cse.want {
			t.Errorf("embedURLFromLink(%q,%q) = %q, want %q", cse.host, cse.link, got, cse.want)
		}
	}
}
