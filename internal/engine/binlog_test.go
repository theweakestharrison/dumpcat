package engine

import (
	"bytes"
	"strings"
	"testing"
)

const sampleOutput = `/*!50530 SET @@SESSION.PSEUDO_SLAVE_MODE=1*/;
# at 383
#261018  3:10:03 server id 11  end_log_pos 425 CRC32 0x8c714b8b 	GTID 0-11-7 ddl
/*M!100001 SET @@session.gtid_domain_id=0*//*!*/;
/*M!100001 SET @@session.server_id=11*//*!*/;
/*M!100001 SET @@session.gtid_seq_no=7*//*!*/;
create table t1.a(id int primary key, note varchar(200) default 'SET @@session.gtid_seq_no=1 stays in data')
/*!*/;
/*!100001 SET @@session.gtid_seq_no=8*//*!*/;
START TRANSACTION
/*!*/;
COMMIT/*!*/;
SET @@session.collation_database=DEFAULT/*!*/;
DROP DATABASE IF EXISTS ` + "`shop`" + `
/*!*/;
create   SCHEMA shop CHARACTER SET utf8mb4
/*!*/;
CREATE DATABASE IF NOT EXISTS ` + "`other`" + ` /* a comment long enough to exceed the sixty-four byte probe window */
/*!*/;
drop table shop.t
# End of log file`

func runFilter(t *testing.T, f *replayFilter, in string, chunk int) string {
	t.Helper()
	var out bytes.Buffer
	f.w = &out
	for i := 0; i < len(in); i += chunk {
		end := min(i+chunk, len(in))
		if n, err := f.Write([]byte(in[i:end])); err != nil || n != end-i {
			t.Fatalf("write: %d %v", n, err)
		}
	}
	if err := f.Flush(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestReplayFilter(t *testing.T) {
	gtidLines := strings.NewReplacer("/*M!100001 SET @@session.gtid_seq_no=7*//*!*/;\n", "", "/*!100001 SET @@session.gtid_seq_no=8*//*!*/;\n", "")
	renamed := strings.NewReplacer(
		"DROP DATABASE IF EXISTS `shop`\n", "DROP DATABASE IF EXISTS `shop_r`\n",
		"create   SCHEMA shop CHARACTER SET utf8mb4\n", "create   SCHEMA `shop_r` CHARACTER SET utf8mb4\n",
		"CREATE DATABASE IF NOT EXISTS `other` /* a comment long enough to exceed the sixty-four byte probe window */\n", "DO 0\n")
	long := strings.Repeat("x", 5000)
	for _, chunk := range []int{1, 2, 7, 63, 64, 65, 4096} {
		for _, in := range []string{sampleOutput, long + "\n" + sampleOutput, sampleOutput + "\n" + long} {
			got := runFilter(t, &replayFilter{stripGTID: true}, in, chunk)
			if exp := strings.Replace(in, sampleOutput, gtidLines.Replace(sampleOutput), 1); got != exp {
				t.Fatalf("gtid, chunk %d: got\n%s\nwant\n%s", chunk, got, exp)
			}
			got = runFilter(t, &replayFilter{from: "shop", to: "shop_r"}, in, chunk)
			if exp := strings.Replace(in, sampleOutput, renamed.Replace(sampleOutput), 1); got != exp {
				t.Fatalf("rename, chunk %d: got\n%s\nwant\n%s", chunk, got, exp)
			}
		}
	}
	// names needing quotes survive the rewrite
	got := runFilter(t, &replayFilter{from: "my`db", to: "new`db"}, "DROP DATABASE `my``db`\n", 3)
	if got != "DROP DATABASE `new``db`\n" {
		t.Fatalf("quoted: %q", got)
	}
}

func TestBinlogReplayArgs(t *testing.T) {
	got := strings.Join(binlogReplayArgs(BinlogStep{File: "/w/mysql-bin.000003", StartPos: 4, Database: "shop"}, false, false), " ")
	if got != "--no-defaults --database=shop /w/mysql-bin.000003" {
		t.Fatalf("plain: %s", got)
	}
	step := BinlogStep{File: "f", StartPos: 120, StopPos: 900, Database: "shop", TargetDB: "shop_restore"}
	got = strings.Join(binlogReplayArgs(step, false, true), " ")
	want := "--no-defaults --start-position=120 --stop-position=900 --rewrite-db=shop_restore->" + rewriteSink + " --rewrite-db=shop->shop_restore --database=shop_restore --skip-gtids f"
	if got != want {
		t.Fatalf("mysql rename:\n%s\n%s", got, want)
	}
	got = strings.Join(binlogReplayArgs(step, true, true), " ")
	want = "--no-defaults --start-position=120 --stop-position=900 --rewrite-db=shop->shop_restore --database=shop f"
	if got != want {
		t.Fatalf("mariadb rename:\n%s\n%s", got, want)
	}
	if got := strings.Join(binlogReplayArgs(BinlogStep{File: "f"}, true, false), " "); got != "--no-defaults f" {
		t.Fatalf("physical: %s", got)
	}
	for _, bad := range []string{"", "-x", "../a", "a/b", `a\b`} {
		if CheckBinlogName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if CheckBinlogName("mysql-bin.000001") != nil {
		t.Error("valid name rejected")
	}
}
