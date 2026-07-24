package main

import (
	"strings"
	"testing"
)

func TestRedactCommand(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"mysql inline password",
			`mysql -h10.0.0.1 -P8806 -uchargeTester -pDayutest@123 -e "SELECT 1"`,
			`mysql -h10.0.0.1 -P8806 -uchargeTester -p*** -e "SELECT 1"`,
		},
		{
			"mysql quoted password",
			`mysql -u root -p's3cret pass' -e "SELECT 1"`,
			`mysql -u root -p*** -e "SELECT 1"`,
		},
		{
			"bearer header",
			`curl -H "Authorization: Bearer tok-abc123" http://x/`,
			`curl -H "Authorization: Bearer ***" http://x/`,
		},
		{
			"token in url",
			`curl "http://x/api?token=abcd1234&page=2"`,
			`curl "http://x/api?token=***&page=2"`,
		},
		{
			"password env",
			`PASSWORD=hunter2 mysql -u root`,
			`PASSWORD=*** mysql -u root`,
		},
	}

	for _, c := range cases {
		got := RedactCommand(c.in)
		if got != c.want {
			t.Errorf("%s:\n got: %s\nwant: %s", c.name, got, c.want)
		}
		if strings.Contains(got, "Dayutest") || strings.Contains(got, "hunter2") || strings.Contains(got, "tok-abc") {
			t.Errorf("%s: secret survived redaction: %s", c.name, got)
		}
	}
}

func TestRedactCommandKeepsBenign(t *testing.T) {
	benign := []string{
		"ls -p /tmp",
		"grep -rn pattern /var/log | tail -5",
		"ls -la /home/work",
	}
	for _, cmd := range benign {
		if got := RedactCommand(cmd); got != cmd {
			t.Errorf("benign command mangled: %q -> %q", cmd, got)
		}
	}
}
