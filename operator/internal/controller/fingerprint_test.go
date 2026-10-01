package controller

import (
	"testing"

	"github.com/ProxySQL/kubernetes/operator/internal/proxysqlclient"
)

func TestSyncFingerprint_ChangesWithSQLStatements(t *testing.T) {
	addrs := []string{"10.0.0.1:6032"}
	base := syncFingerprint(&proxysqlclient.Desired{}, addrs)
	withStmt := syncFingerprint(&proxysqlclient.Desired{
		SQLStatements: []string{"PROXYSQL FLUSH QUERY CACHE"},
	}, addrs)
	if base == withStmt {
		t.Fatal("adding sqlStatements must change the sync fingerprint")
	}
	edited := syncFingerprint(&proxysqlclient.Desired{
		SQLStatements: []string{"PROXYSQL FLUSH MYSQL QUERY CACHE"},
	}, addrs)
	if withStmt == edited {
		t.Fatal("editing a statement must change the sync fingerprint")
	}
}

func TestSyncFingerprint_ChangesWithGaleraHostgroups(t *testing.T) {
	addrs := []string{"10.0.0.1:6032"}
	base := syncFingerprint(&proxysqlclient.Desired{}, addrs)
	withRow := syncFingerprint(&proxysqlclient.Desired{
		MySQLGaleraHostgroups: []proxysqlclient.MySQLGaleraHostgroup{{
			WriterHostgroup: 10, BackupWriterHostgroup: 12,
			ReaderHostgroup: 11, OfflineHostgroup: 13,
		}},
	}, addrs)
	if base == withRow {
		t.Fatal("adding a galera hostgroup row must change the sync fingerprint")
	}
}
