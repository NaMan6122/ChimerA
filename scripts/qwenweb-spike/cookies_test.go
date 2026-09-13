package main

import "testing"

// Golden vectors generated with qwen-reverse 0.1.6 cookies.py custom_encode(s, True):
//
//	python3 -c "from qp.cookies import custom_encode; print(custom_encode(S, True))"
func TestCustomEncodeGolden(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "4"},
		{"a", "eID"},
		{"hello world", "GK3qjO0kDY01oitqGQx"},
		{"tool", "i5zwG=x"},
		{"Line1-Line2", "0ebqUxpx2D6WoiKx"},
		{"ABCDEFGHIJ0123456789xyz", "eee4qxexPxKx5xi4meioDQG7DRDIxiqi=D9DCqDtDgDG5izDSoD"},
		{"0^1^2^3^4^5^6^7^8^9", "Dql42iGQeQqxniDciD9iDyiDBiDgYD"},
	}
	for _, c := range cases {
		if got := customEncode(c.in, true); got != c.want {
			t.Errorf("customEncode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
