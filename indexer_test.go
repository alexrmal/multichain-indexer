package main

import "testing"

func TestFindForkPointFrom(t *testing.T) {
	cases := []struct {
		name     string
		from     int64
		maxDepth int64
		remote   map[int64]string
		local    map[int64]string
		want     int64
		wantErr  bool
	}{
		{
			name:     "no reorg - immediate match",
			from:     10,
			maxDepth: 20,
			remote:   map[int64]string{10: "a"},
			local:    map[int64]string{10: "a"},
			want:     10,
		},
		{
			name:     "shallow reorg - 3 blocks back",
			from:     10,
			maxDepth: 20,
			remote:   map[int64]string{10: "x", 9: "x", 8: "x", 7: "match"},
			local:    map[int64]string{10: "a", 9: "b", 8: "c", 7: "match"},
			want:     7,
		},
		{
			name:     "reorg exactly at maxDepth boundary",
			from:     20,
			maxDepth: 5,
			remote:   map[int64]string{20: "x", 19: "x", 18: "x", 17: "x", 16: "match"},
			local:    map[int64]string{20: "a", 19: "b", 18: "c", 17: "d", 16: "match"},
			want:     16,
		},
		{
			name:     "reorg past maxDepth - error",
			from:     20,
			maxDepth: 3,
			remote:   map[int64]string{20: "x", 19: "x", 18: "x"},
			local:    map[int64]string{20: "a", 19: "b", 18: "c"},
			wantErr:  true,
		},
		{
			name:     "genesis edge case - walk back to 0",
			from:     3,
			maxDepth: 20,
			remote:   map[int64]string{3: "x", 2: "x", 1: "x"},
			local:    map[int64]string{3: "a", 2: "b", 1: "c"},
			want:     0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			remoteHash := func(n int64) (string, error) { return tc.remote[n], nil }
			localHash := func(n int64) (string, error) { return tc.local[n], nil }

			got, err := findForkPointFrom(tc.from, tc.maxDepth, remoteHash, localHash)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got forkPoint=%d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("forkPoint = %d, want %d", got, tc.want)
			}
		})
	}
}
