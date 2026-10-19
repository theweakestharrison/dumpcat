package engine

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPortableRenameSchema(t *testing.T) {
	in := "/*!50003 CREATE*/ /*!50017 DEFINER=`root`@`localhost`*/ /*!50003 TRIGGER shop.item_ai AFTER INSERT ON shop.items FOR EACH ROW UPDATE `shop`.`orders` SET n = 1 */;;\n" +
		"INSERT INTO `orders` VALUES (1,'shop.items and `shop`.x stay as they are');\n" +
		"CREATE TABLE `t` (`a` int COMMENT 'myshop.x', `b` int) ENGINE=InnoDB;\n" +
		"/*!50001 VIEW `v` AS select `shop2`.`t`.`a` AS `a` from `shop2`.`t` */;\n"
	b, err := io.ReadAll(PortableSQL(strings.NewReader(in), PortableOpts{RenameFrom: "shop", RenameTo: "shop__drill_3"}))
	if err != nil {
		t.Fatal(err)
	}
	want := "/*!50003 CREATE*/ /*!50017 DEFINER=`root`@`localhost`*/ /*!50003 TRIGGER `shop__drill_3`.item_ai AFTER INSERT ON `shop__drill_3`.items FOR EACH ROW UPDATE `shop__drill_3`.`orders` SET n = 1 */;;\n" +
		"INSERT INTO `orders` VALUES (1,'shop.items and `shop`.x stay as they are');\n" +
		"CREATE TABLE `t` (`a` int COMMENT 'myshop.x', `b` int) ENGINE=InnoDB;\n" +
		"/*!50001 VIEW `v` AS select `shop2`.`t`.`a` AS `a` from `shop2`.`t` */;\n"
	if string(b) != want {
		t.Fatalf("got\n%s\nwant\n%s", b, want)
	}
	// names that need quoting are only matched quoted
	b, _ = io.ReadAll(PortableSQL(strings.NewReader("CREATE TRIGGER `my-db`.`t` AFTER INSERT ON `my-db`.`x` FOR EACH ROW SET @a = 1;\n"), PortableOpts{RenameFrom: "my-db", RenameTo: "my-db__drill_1"}))
	if got := string(b); got != "CREATE TRIGGER `my-db__drill_1`.`t` AFTER INSERT ON `my-db__drill_1`.`x` FOR EACH ROW SET @a = 1;\n" {
		t.Fatalf("got %s", got)
	}
}

func TestRenameMydumperSchemas(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"shop-schema-create.sql":         "CREATE DATABASE `shop`;\n",
		"shop.items-schema-triggers.sql": "CREATE TRIGGER shop.ai AFTER INSERT ON shop.items FOR EACH ROW SET @a=1;\n",
		"shop-schema-post.sql":           "CREATE PROCEDURE `shop`.`p`() SELECT 1;\n",
		"crm.items-schema-triggers.sql":  "CREATE TRIGGER crm.ai AFTER INSERT ON crm.items FOR EACH ROW SET @a=1;\n",
		"shop.items.00000.sql":           "INSERT INTO `items` VALUES(1,\"shop.x\");\n",
	}
	for n, body := range files {
		os.WriteFile(filepath.Join(dir, n), []byte(body), 0o600)
	}
	n, err := RenameMydumperSchemas(dir, map[string]string{"shop": "shop__drill_9", "crm": "crm"})
	if err != nil || n != 2 {
		t.Fatalf("rewrote %d files: %v", n, err)
	}
	read := func(n string) string { b, _ := os.ReadFile(filepath.Join(dir, n)); return string(b) }
	if got := read("shop.items-schema-triggers.sql"); !strings.Contains(got, "TRIGGER `shop__drill_9`.ai AFTER INSERT ON `shop__drill_9`.items") {
		t.Fatalf("trigger: %s", got)
	}
	if got := read("shop-schema-post.sql"); !strings.Contains(got, "`shop__drill_9`.`p`") {
		t.Fatalf("routine: %s", got)
	}
	for _, n := range []string{"shop-schema-create.sql", "crm.items-schema-triggers.sql", "shop.items.00000.sql"} {
		if read(n) != files[n] {
			t.Fatalf("%s must be left alone: %s", n, read(n))
		}
	}
}
