package store

import "testing"

func TestParseDateArg(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		{"empty is unbounded", "", 0, false},
		{"bare date, UTC midnight", "2023-11-14", 1699920000000, false},
		{"RFC3339 with time", "2023-11-14T12:00:00Z", 1699963200000, false},
		{"raw epoch milliseconds", "1699920000000", 1699920000000, false},
		{"garbage", "not-a-date", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseDateArg(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParseDateArg(%q) = %d, nil; want an error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDateArg(%q) returned an error: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("ParseDateArg(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}
