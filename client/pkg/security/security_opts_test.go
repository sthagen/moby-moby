package security

import (
	"fmt"
	"testing"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/assert/cmp"
)

func TestDecode(t *testing.T) {
	tests := []struct {
		name    string
		opts    []string
		want    []Option
		wantErr string
	}{
		{
			name: "empty options",
			opts: []string{},
			want: []Option{},
		},
		{
			name: "nil options",
			opts: nil,
			want: []Option{},
		},
		{
			name: "legacy format without equals",
			opts: []string{"apparmor:unconfined"},
			want: []Option{
				{Name: "apparmor:unconfined"},
			},
		},
		{
			name: "single option with name only",
			opts: []string{"name=apparmor"},
			want: []Option{
				{Name: "apparmor"},
			},
		},
		{
			name: "single option with name and additional options",
			opts: []string{"name=selinux,type=container_t,level=s0:c1.c2"},
			want: []Option{
				{
					Name: "selinux",
					Options: []KeyValue{
						{Key: "type", Value: "container_t"},
						{Key: "level", Value: "s0:c1.c2"},
					},
				},
			},
		},
		{
			name: "multiple options",
			opts: []string{
				"name=apparmor,profile=docker-default",
				"name=seccomp,profile=unconfined",
			},
			want: []Option{
				{
					Name: "apparmor",
					Options: []KeyValue{
						{Key: "profile", Value: "docker-default"},
					},
				},
				{
					Name: "seccomp",
					Options: []KeyValue{
						{Key: "profile", Value: "unconfined"},
					},
				},
			},
		},
		{
			name: "mixed legacy and new format",
			opts: []string{
				"label:disable",
				"name=apparmor,profile=custom",
			},
			want: []Option{
				{Name: "label:disable"},
				{
					Name: "apparmor",
					Options: []KeyValue{
						{Key: "profile", Value: "custom"},
					},
				},
			},
		},
		{
			name: "option without name key",
			opts: []string{"profile=custom,type=container_t"},
			want: []Option{
				{
					Options: []KeyValue{
						{Key: "profile", Value: "custom"},
						{Key: "type", Value: "container_t"},
					},
				},
			},
		},
		{
			name: "option with equals in value",
			opts: []string{"name=selinux,level=s0:c1=c2"},
			want: []Option{
				{
					Name: "selinux",
					Options: []KeyValue{
						{Key: "level", Value: "s0:c1=c2"},
					},
				},
			},
		},
		{
			name:    "invalid option without equals in comma-separated list",
			opts:    []string{"name=apparmor,invalid"},
			wantErr: `invalid security option "invalid"`,
		},
		{
			name:    "empty key",
			opts:    []string{"=value"},
			wantErr: "invalid empty security option",
		},
		{
			name:    "empty value",
			opts:    []string{"key="},
			wantErr: "invalid empty security option",
		},
		{
			name:    "empty key and value",
			opts:    []string{"="},
			wantErr: "invalid empty security option",
		},
		{
			name:    "empty key in middle",
			opts:    []string{"name=apparmor,=value"},
			wantErr: "invalid empty security option",
		},
		{
			name:    "empty value in middle",
			opts:    []string{"name=apparmor,key="},
			wantErr: "invalid empty security option",
		},
		{
			name: "complex real-world example",
			opts: []string{
				"name=selinux,user=system_u,role=system_r,type=container_t,level=s0:c1.c2",
				"name=apparmor,profile=/usr/bin/docker",
				"name=seccomp,profile=builtin",
			},
			want: []Option{
				{
					Name: "selinux",
					Options: []KeyValue{
						{Key: "user", Value: "system_u"},
						{Key: "role", Value: "system_r"},
						{Key: "type", Value: "container_t"},
						{Key: "level", Value: "s0:c1.c2"},
					},
				},
				{
					Name: "apparmor",
					Options: []KeyValue{
						{Key: "profile", Value: "/usr/bin/docker"},
					},
				},
				{
					Name: "seccomp",
					Options: []KeyValue{
						{Key: "profile", Value: "builtin"},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeOptions(tc.opts)

			if tc.wantErr == "" {
				assert.NilError(t, err)
				assert.Check(t, cmp.DeepEqual(got, tc.want))
			} else {
				assert.Check(t, err != nil, "expected error but got none")
				assert.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
}

func BenchmarkDecode(b *testing.B) {
	opts := []string{
		"name=selinux,user=system_u,role=system_r,type=container_t,level=s0:c1.c2",
		"name=apparmor,profile=/usr/bin/docker",
		"name=seccomp,profile=builtin",
		"legacy:format",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := DecodeOptions(opts)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeLegacy(b *testing.B) {
	opts := []string{
		"apparmor:unconfined",
		"label:disable",
		"seccomp:unconfined",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := DecodeOptions(opts)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeComplex(b *testing.B) {
	opts := make([]string, 100)
	for i := range opts {
		opts[i] = fmt.Sprintf("name=test%d,key1=value1,key2=value2,key3=value3", i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := DecodeOptions(opts)
		if err != nil {
			b.Fatal(err)
		}
	}
}
