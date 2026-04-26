package ssh

import "testing"

func TestBinaryArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   Target
		want []string
	}{
		{
			name: "ssh config name",
			in:   Target{Name: "prod"},
			want: []string{"prod"},
		},
		{
			name: "explicit host only",
			in:   Target{Host: "host"},
			want: []string{"host"},
		},
		{
			name: "explicit default port",
			in:   Target{User: "alice", Host: "host", Port: 22},
			want: []string{"alice@host"},
		},
		{
			name: "explicit custom port",
			in:   Target{User: "alice", Host: "host", Port: 2222},
			want: []string{"-p", "2222", "alice@host"},
		},
		{
			name: "name wins",
			in:   Target{Name: "prod", User: "alice", Host: "host", Port: 2222},
			want: []string{"prod"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BinaryArgs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
