package namespaceresolvers

import (
	"testing"
	"time"
)

func TestResolverConfigPreservesSearchAndControlsForwarding(t *testing.T) {
	config, err := parseResolverConfig([]byte("nameserver 127.0.0.53\nnameserver 169.254.1.253\nsearch svc.example\noptions rotate timeout:2 attempts:3 ndots:2 use-vc single-request single-request-reopen no-tld-query edns0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(config.nameservers) != 2 || !config.rotate || !config.useTCP ||
		config.timeout != 2*time.Second || config.attempts != 3 {
		t.Fatalf("resolver config = %#v", config)
	}
	child := string(resolverText(config.nameservers[:1], config.suffix))
	want := "nameserver 127.0.0.53\nsearch svc.example\noptions ndots:2 single-request single-request-reopen no-tld-query edns0\n"
	if child != want {
		t.Fatalf("child resolver = %q", child)
	}
}

func TestResolverConfigRejectsUnknownOrUnboundedInput(t *testing.T) {
	for _, content := range []string{
		"lookup file bind\nnameserver 192.0.2.53\n",
		"nameserver 192.0.2.53\noptions timeout:99\n",
		"nameserver 192.0.2.53\noptions unknown\n",
		"nameserver 192.0.2.53\noptions trust-ad\n",
		"search bad/domain\nnameserver 192.0.2.53\n",
		"domain one.example two.example\nnameserver 192.0.2.53\n",
		"domain one.example\ndomain two.example\nnameserver 192.0.2.53\n",
		"search one.example\nsearch two.example\nnameserver 192.0.2.53\n",
		"domain one.example\nsearch two.example\nnameserver 192.0.2.53\n",
		"search one.example\ndomain two.example\nnameserver 192.0.2.53\n",
		"nameserver 192.0.2.1\nnameserver 192.0.2.2\nnameserver 192.0.2.3\nnameserver invalid\n",
	} {
		if _, err := parseResolverConfig([]byte(content)); err == nil {
			t.Fatalf("invalid resolver configuration was accepted: %q", content)
		}
	}
}

func TestResolverConfigUsesFirstThreeValidNameservers(t *testing.T) {
	config, err := parseResolverConfig([]byte(
		"nameserver 192.0.2.1\n" +
			"nameserver 192.0.2.2\n" +
			"nameserver 192.0.2.3\n" +
			"nameserver 192.0.2.4\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(config.nameservers); got != maximumNameservers {
		t.Fatalf("effective nameservers=%d want=%d", got, maximumNameservers)
	}
	for index, want := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"} {
		if got := config.nameservers[index].String(); got != want {
			t.Fatalf("effective nameserver %d=%s want=%s", index, got, want)
		}
	}
}

func TestResolverConfigAcceptsOneDomainOrSearchDirective(t *testing.T) {
	for _, content := range []string{
		"domain one.example\nnameserver 192.0.2.53\n",
		"search one.example two.example\nnameserver 192.0.2.53\n",
	} {
		if _, err := parseResolverConfig([]byte(content)); err != nil {
			t.Fatalf("valid resolver configuration %q: %v", content, err)
		}
	}
}
