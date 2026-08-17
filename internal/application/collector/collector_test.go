package collector

import "testing"

func TestExtractBitrateParsesAdvertisedMbps(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"📦 71.2 GB 📊 62.4 Mbps", 62_400_000},
		{"📊 6.34 Mbps 📡 Torrentio", 6_340_000},
		{"📊 31.7 Mbps", 31_700_000},
		{"no bitrate here", 0},
		{"", 0},
	}
	for _, c := range cases {
		if got := extractBitrate(c.in); got != c.want {
			t.Errorf("extractBitrate(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
