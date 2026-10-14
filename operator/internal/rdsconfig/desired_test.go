package rdsconfig

import (
	"testing"

	"github.com/ProxySQL/kubernetes/operator/internal/rdsclient"
)

func TestDesiredDriftNormalizesOrderAndDefaults(t *testing.T) {
	desired := Desired{
		Proxy: rdsclient.Proxy{Name: "orders", ARN: "arn:proxy", EngineFamily: "MYSQL", DefaultAuthScheme: "NONE", IdleClientTimeout: 1800,
			VpcSecurityGroupIDs: []string{"sg-b", "sg-a"}, Auth: []rdsclient.Auth{}},
		TargetGroup: rdsclient.TargetGroup{Name: "default", Pool: rdsclient.Pool{ConnectionBorrowTimeout: 120, MaxConnectionsPercent: 100, MaxIdleConnectionsPercent: 50}},
		Targets:     []rdsclient.Target{{Identifier: "db-b", Type: "RDS_INSTANCE"}, {Identifier: "db-a", Type: "RDS_INSTANCE"}},
		Tags:        []rdsclient.Tag{{Key: "team", Value: "payments"}},
	}
	observed := rdsclient.State{
		Proxy: rdsclient.Proxy{Name: "orders", ARN: "arn:proxy", EngineFamily: "MYSQL", DefaultAuthScheme: "NONE", IdleClientTimeout: 1800,
			VpcSecurityGroupIDs: []string{"sg-a", "sg-b"}, Auth: []rdsclient.Auth{}},
		TargetGroup: desired.TargetGroup,
		Targets:     []rdsclient.Target{{Identifier: "db-a", Type: "RDS_INSTANCE"}, {Identifier: "db-b", Type: "RDS_INSTANCE"}},
		Tags:        desired.Tags,
	}
	if drift := Drift(desired, observed); len(drift) != 0 {
		t.Fatalf("Drift = %v, want none", drift)
	}
	observed.Targets = nil
	if drift := Drift(desired, observed); len(drift) != 1 || drift[0] != "targets" {
		t.Fatalf("Drift = %v, want targets", drift)
	}
}
