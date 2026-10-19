package engine

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"dumpcat/internal/store"
)

const mysqldumpSample = "-- MariaDB dump 10.19-11.4.13-MariaDB, for Win64 (AMD64)\n" +
	"--\n-- Host: 127.0.0.1    Database: shop\n" +
	"-- ------------------------------------------------------\n" +
	"/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;\n" +
	"/*!40101 SET NAMES utf8mb4 */;\n\n" +
	"--\n-- Table structure for table `orders`\n--\n\n" +
	"DROP TABLE IF EXISTS `orders`;\n" +
	"/*!40101 SET @saved_cs_client     = @@character_set_client */;\n" +
	"CREATE TABLE `orders` (\n  `id` int(11) NOT NULL,\n  `note` varchar(50) DEFAULT 'a;b' COMMENT 'x(y',\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n" +
	"LOCK TABLES `orders` WRITE;\n/*!40000 ALTER TABLE `orders` DISABLE KEYS */;\n" +
	"INSERT INTO `orders` VALUES (1,'it''s (a); test'),(2,'back\\\\slash \\' quote'),(3,\"dq ) ( \\\" ;\"),(4,NULL);\n" +
	"INSERT INTO `orders` VALUES (5,'/* not a comment */'),(6,'-- nor this');\n" +
	"/*!40000 ALTER TABLE `orders` ENABLE KEYS */;\nUNLOCK TABLES;\n\n" +
	"CREATE TABLE `empty` (`id` int) ENGINE=InnoDB;\n" +
	"CREATE TABLE `odd``name` (`id` int);\n" +
	"INSERT IGNORE INTO `odd``name` (`id`) VALUES (1),(2);\n" +
	"--\n-- Temporary table structure for view `v`\n--\n" +
	"/*!50001 CREATE VIEW `v` AS SELECT\n 1 AS `id` */;\n" +
	"/*!50001 CREATE TABLE `v_old` (\n  `id` tinyint NOT NULL\n) ENGINE=MyISAM */;\n" +
	"DELIMITER ;;\n" +
	"CREATE DEFINER=`root`@`localhost` PROCEDURE `p`()\nBEGIN\n  INSERT INTO orders VALUES (99,'x');\nEND ;;\n" +
	"/*!50003 CREATE*/ /*!50017 DEFINER=`root`@`%`*/ /*!50003 TRIGGER t AFTER INSERT ON orders FOR EACH ROW INSERT INTO empty VALUES (1) */;;\n" +
	"DELIMITER ;\n" +
	"-- Dump completed on 2026-10-19 10:00:00\n"

func TestRowCounterMysqldump(t *testing.T) {
	want := store.TableStats{"shop": {"orders": 6, "empty": 0, "odd`name": 2}}
	// whole, then byte by byte: state must survive any write boundary
	for _, chunk := range []int{len(mysqldumpSample), 1, 7} {
		rc := NewRowCounter("shop")
		s := []byte(mysqldumpSample)
		for len(s) > 0 {
			n := min(chunk, len(s))
			rc.Write(s[:n])
			s = s[n:]
		}
		if got := rc.Tables(); !reflect.DeepEqual(got, want) {
			t.Fatalf("chunk %d: got %v want %v", chunk, got, want)
		}
	}
}

func TestRowCounterSingleFile(t *testing.T) {
	dump := "CREATE DATABASE /*!32312 IF NOT EXISTS*/ `a` /*!40100 DEFAULT CHARACTER SET utf8mb4 */;\nUSE `a`;\n" +
		"CREATE TABLE `t` (`id` int);\nINSERT INTO `t` VALUES (1),(2);\n" +
		"CREATE DATABASE `b`;\nUSE `b`;\nCREATE TABLE `t` (`id` int);\nINSERT INTO `t` VALUES (1);\nINSERT INTO `t` VALUES (2),(3),(4);\n"
	rc := NewRowCounter("")
	rc.Write([]byte(dump))
	want := store.TableStats{"a": {"t": 2}, "b": {"t": 4}}
	if got := rc.Tables(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestMydumperTableStats(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, gz bool) {
		p := filepath.Join(dir, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if gz {
			w := gzip.NewWriter(f)
			w.Write([]byte(body))
			w.Close()
			return
		}
		f.WriteString(body)
	}
	write("metadata", "[source]\nFile = bin.000001\n", false)
	write("shop-schema-create.sql", "CREATE DATABASE `shop`;\n", false)
	write("shop.orders-schema.sql.gz", "/*!40101 SET NAMES binary*/;\nCREATE TABLE `orders` (\n `id` int\n);\n", true)
	write("shop.orders.00000.sql.gz", "/*!40101 SET NAMES binary*/;\nINSERT INTO `orders` VALUES(1,\"a\")\n,(2,\"b(\")\n;\n", true)
	write("shop.orders.00001.sql.gz", "INSERT INTO `orders` VALUES(3,\"c\");\n", true)
	write("shop.empty-schema.sql", "CREATE TABLE `empty` (`id` int);\n", false)
	write("shop.v-schema.sql", "CREATE TABLE `v`(`id` tinyint NOT NULL) ENGINE=MEMORY;\n", false)
	write("shop.v-schema-view.sql", "CREATE VIEW `v` AS select 1 AS `id`;\n", false)
	write("shop.orders-schema-triggers.sql", "CREATE TRIGGER x AFTER INSERT ON orders FOR EACH ROW INSERT INTO empty VALUES (1);\n", false)
	write("shop.archive.logs-schema.sql", "CREATE TABLE `logs` (`id` int);\n", false)
	write("shop.archive.logs.00000.sql", "INSERT INTO `logs` VALUES(1),(2);\n", false)
	got, err := MydumperTableStats(dir, []string{"shop", "shop.archive"})
	if err != nil {
		t.Fatal(err)
	}
	want := store.TableStats{"shop": {"orders": 3, "empty": 0}, "shop.archive": {"logs": 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	write("shop.orders.00002.dat", "4\td\n", false)
	if got, _ := MydumperTableStats(dir, []string{"shop", "shop.archive"}); got != nil {
		t.Fatalf("LOAD DATA files must disable the counts, got %v", got)
	}
}
