package intake

import "testing"

func TestCheckSource(t *testing.T) {
	ok := []string{
		"https://docs.datadoghq.com/account_management/api-app-keys/",
		"https://example.com:8443/a?b=c#d",
		"https://93.184.215.14/",
		"https://[2606:2800:21f:cb07:6820:80da:af6b:8b2c]/",
		"https://xn--bcher-kva.example/",
		"  https://example.com/  ",
	}
	for _, s := range ok {
		if err := CheckSource(s); err != nil {
			t.Errorf("CheckSource(%q) = %v; want accepted", s, err)
		}
	}
	bad := []string{
		"",
		"file:///etc/passwd",
		"git://example.com/repo.git",
		"http://example.com/",
		"ftp://example.com/",
		"HTTPS//example.com",
		"https:example.com",
		"https:///path",
		"https://user:pw@example.com/",
		"https://localhost/",
		"https://api.localhost./",
		"https://127.0.0.1/",
		"https://127.1/",
		"https://2130706433/",
		"https://0x7f.0.0.1/",
		"https://0x7f000001/",
		"https://169.254.169.254/latest/meta-data/",
		"https://10.0.0.8/",
		"https://172.16.3.4/",
		"https://192.168.1.1/",
		"https://100.64.1.1/",
		"https://0.0.0.0/",
		"https://224.0.0.1/",
		"https://255.255.255.255/",
		"https://[::1]/",
		"https://[::]/",
		"https://[fe80::1]/",
		"https://[fe80::1%25en0]/",
		"https://[fd00::1]/",
		"https://[::ffff:127.0.0.1]/",
		"https://[::ffff:169.254.169.254]/",
		"https://[::7f00:1]/",
		"https://[64:ff9b::a9fe:a9fe]/",
		"https://[2002:a9fe:a9fe::]/",
		"https://[ff02::1]/",
		"https://exa..mple.com/",
	}
	for _, s := range bad {
		if err := CheckSource(s); err == nil {
			t.Errorf("CheckSource(%q) accepted", s)
		}
	}
}
