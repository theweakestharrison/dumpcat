package engine

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

const sampleDump = "-- MySQL dump 10.13\n" +
	"SET @MYSQLDUMP_TEMP_LOG_BIN = @@SESSION.SQL_LOG_BIN;\n" +
	"SET @@SESSION.SQL_LOG_BIN= 0;\n" +
	"SET @@GLOBAL.GTID_PURGED=/*!80000 '+'*/ '3e11fa47-71ca-11e1-9e33-c80aa9429562:1-5,\n" +
	"4f22ab58-71ca-11e1-9e33-c80aa9429562:1-3';\n" +
	"CREATE TABLE `t` (\n" +
	"  `name` varchar(20) COLLATE utf8mb4_0900_as_cs DEFAULT NULL,\n" +
	"  `code` varchar(20) CHARACTER SET utf8mb3 COLLATE utf8mb3_general_ci\n" +
	") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;\n" +
	"INSERT INTO `t` VALUES ('DEFINER=`x`@`y` utf8mb4_0900_ai_ci','a');\n" +
	"/*!50013 DEFINER=`root`@`localhost` SQL SECURITY DEFINER */\n" +
	"/*!50003 CREATE*/ /*!50017 DEFINER=`app`@`%`*/ /*!50003 TRIGGER `trg` BEFORE INSERT ON `t` FOR EACH ROW SET NEW.code = 'x' */;;\n" +
	"CREATE DEFINER='odd``user'@'10.0.0.%' PROCEDURE `p`()\n" +
	"/*!50003 SET collation_connection  = utf8mb4_uca1400_vietnamese_ai_ci */ ;\n" +
	"SET @@SESSION.SQL_LOG_BIN = @MYSQLDUMP_TEMP_LOG_BIN;\n"

func portable(t *testing.T, in string, o PortableOpts) string {
	t.Helper()
	b, err := io.ReadAll(PortableSQL(strings.NewReader(in), o))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPortableSQLAll(t *testing.T) {
	got := portable(t, sampleDump, PortableOpts{StripDefiner: true, ConvertCollations: true, SkipGTIDPurged: true})
	for _, bad := range []string{"GTID_PURGED=", "4f22ab58", "SQL_LOG_BIN", "DEFINER=`root`", "DEFINER=`app`", "odd``user", "0900_ai_ci DEFAULT", "COLLATE=utf8mb4_0900", "uca1400", "utf8mb3_general_ci"} {
		if strings.Contains(got, bad) {
			t.Errorf("output still contains %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{
		"-- SET @@GLOBAL.GTID_PURGED skipped by DumpCat\n",
		"COLLATE utf8mb4_bin DEFAULT NULL",
		"CHARACTER SET utf8mb3 COLLATE utf8_general_ci",
		"COLLATE=utf8mb4_unicode_ci;",
		// data lines are never rewritten
		"INSERT INTO `t` VALUES ('DEFINER=`x`@`y` utf8mb4_0900_ai_ci','a');\n",
		"/*!50013 SQL SECURITY DEFINER */\n",
		"/*!50017 */ /*!50003 TRIGGER",
		"CREATE PROCEDURE `p`()\n",
		"collation_connection  = utf8mb4_unicode_ci */",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestPortableSQLOnlyRequested(t *testing.T) {
	if got := portable(t, sampleDump, PortableOpts{}); got != sampleDump {
		t.Fatal("no options must leave the dump unchanged")
	}
	got := portable(t, sampleDump, PortableOpts{StripDefiner: true})
	if !strings.Contains(got, "GTID_PURGED=") || !strings.Contains(got, "utf8mb4_0900_ai_ci;") {
		t.Fatal("only DEFINER must be rewritten")
	}
	if strings.Contains(got, "DEFINER=`root`") {
		t.Fatal("DEFINER not stripped")
	}
}

func TestPortableSQLLongLines(t *testing.T) {
	// a data line far larger than the read buffer is copied verbatim
	row := "INSERT INTO `t` VALUES ('" + strings.Repeat("utf8mb4_0900_ai_ci DEFINER=`a`@`b` ", 20000) + "');\n"
	// so is a long DDL line, which is still rewritten
	view := "/*!50001 VIEW `v` AS select '" + strings.Repeat("x", 200000) + "' COLLATE utf8mb4_0900_ai_ci */;\n"
	in := row + view + "SET @@GLOBAL.GTID_PURGED='a:1';\n" + row
	got := portable(t, in, PortableOpts{StripDefiner: true, ConvertCollations: true, SkipGTIDPurged: true})
	want := row + strings.Replace(view, "utf8mb4_0900_ai_ci", "utf8mb4_unicode_ci", 1) + "-- SET @@GLOBAL.GTID_PURGED skipped by DumpCat\n" + row
	if got != want {
		t.Fatalf("long lines mangled (got %d bytes, want %d)", len(got), len(want))
	}
	// no trailing newline at EOF
	if got := portable(t, "CREATE TABLE x COLLATE=utf8mb4_0900_ai_ci", PortableOpts{ConvertCollations: true}); got != "CREATE TABLE x COLLATE=utf8mb4_unicode_ci" {
		t.Fatalf("last line: %q", got)
	}
}

func TestPortableCollation(t *testing.T) {
	cases := map[string]string{
		"utf8mb4_0900_ai_ci":          "utf8mb4_unicode_ci",
		"utf8mb4_0900_bin":            "utf8mb4_bin",
		"utf8mb4_vi_0900_as_cs":       "utf8mb4_bin",
		"utf8mb4_uca1400_ai_ci":       "utf8mb4_unicode_ci",
		"utf8mb4_uca1400_nopad_as_cs": "utf8mb4_bin",
		"utf8mb3_uca1400_ai_ci":       "utf8_unicode_ci",
		"utf8mb3_general_ci":          "utf8_general_ci",
		"utf8mb3_bin":                 "utf8_bin",
		"utf8mb4_general_ci":          "utf8mb4_general_ci",
		"utf8mb4_unicode_520_ci":      "utf8mb4_unicode_520_ci",
		"latin1_swedish_ci":           "latin1_swedish_ci",
		"":                            "",
	}
	for in, want := range cases {
		if got := PortableCollation(in); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}

func TestRewriteMydumperSchemas(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, compress bool) {
		var b bytes.Buffer
		if compress {
			zw, _ := zstd.NewWriter(&b)
			zw.Write([]byte(body))
			zw.Close()
		} else {
			b.WriteString(body)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b.Bytes(), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	ddl := "CREATE TABLE `t` (`a` int) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;\n"
	data := "INSERT INTO `t` VALUES\n('utf8mb4_0900_ai_ci');\n"
	write("shop.t-schema.sql.zst", ddl, true)
	write("shop-schema-create.sql", "CREATE DATABASE `shop` COLLATE utf8mb4_0900_ai_ci;\n", false)
	write("shop.v-schema-view.sql.zst", "/*!50013 DEFINER=`root`@`localhost` SQL SECURITY DEFINER */\n", true)
	write("shop.t.00000.sql.zst", data, true)
	write("metadata", "utf8mb4_0900_ai_ci", false)
	n, err := RewriteMydumperSchemas(dir, PortableOpts{StripDefiner: true, ConvertCollations: true})
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if _, err := os.Stat(filepath.Join(dir, "shop.t-schema.sql.zst")); !os.IsNotExist(err) {
		t.Fatal("the compressed original must be replaced")
	}
	if got := read("shop.t-schema.sql"); !strings.Contains(got, "COLLATE=utf8mb4_unicode_ci") {
		t.Fatalf("table schema: %q", got)
	}
	if got := read("shop-schema-create.sql"); !strings.Contains(got, "COLLATE utf8mb4_unicode_ci") {
		t.Fatalf("database schema: %q", got)
	}
	if got := read("shop.v-schema-view.sql"); strings.Contains(got, "DEFINER=") {
		t.Fatalf("view schema: %q", got)
	}
	if read("metadata") != "utf8mb4_0900_ai_ci" {
		t.Fatal("non-schema files must not be touched")
	}
	if _, err := os.Stat(filepath.Join(dir, "shop.t.00000.sql.zst")); err != nil {
		t.Fatal("data files must not be touched")
	}
}
