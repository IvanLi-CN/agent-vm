package deployd

import "testing"

func TestConfigRequiresTLSAndACLForNonLoopbackMetrics(t *testing.T) {
	config := Defaults()
	config.MetricsListen = "192.0.2.10:9911"
	if err := config.Validate(); err == nil {
		t.Fatal("non-loopback metrics listener was accepted without mTLS and ACL")
	}
	config.NetworkACL = "192.0.2.0/24,2001:db8::10"
	config.MetricsTLS = MetricsTLSConfig{
		Certificate: "/etc/agent-vm-native/tls/deployd.crt",
		PrivateKey:  "/etc/agent-vm-native/tls/deployd.key",
		ClientCA:    "/etc/agent-vm-native/tls/ca.crt",
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRejectsInvalidMetricsACLAndPartialTLS(t *testing.T) {
	config := Defaults()
	config.NetworkACL = "not-a-cidr"
	if err := config.Validate(); err == nil {
		t.Fatal("invalid loopback ACL was accepted")
	}
	config = Defaults()
	config.MetricsTLS.Certificate = "/tmp/deployd.crt"
	if err := config.Validate(); err == nil {
		t.Fatal("partial TLS configuration was accepted")
	}
}

func TestConfigRejectsNonSingletonLockPath(t *testing.T) {
	config := Defaults()
	config.LockPath = "/tmp/second-deployd.lock"
	if err := config.Validate(); err == nil {
		t.Fatal("deployd accepted a configurable singleton lock path")
	}
}

func TestMetricsAddressRequiresTLSTreatsWildcardAsNonLoopback(t *testing.T) {
	for _, address := range []string{":9911", "0.0.0.0:9911", "[::]:9911", "192.0.2.10:9911"} {
		if !MetricsAddressRequiresTLS(address) {
			t.Fatalf("metrics address %q was not treated as non-loopback", address)
		}
	}
	if MetricsAddressRequiresTLS("127.0.0.1:9911") || MetricsAddressRequiresTLS("[::1]:9911") {
		t.Fatal("loopback metrics address unexpectedly requires TLS")
	}
}
