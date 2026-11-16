package main

// Восстановление метаданного файла коллекции chromem-go v0.7.0.
//
// NewPersistentDB при старте требует, чтобы в каталоге коллекции был файл
// 00000000.gob.gz (<hash2hex(name)>) с именем и метаданными коллекции.
// Если он потерян, открытие стора падает с "collection metadata file not
// found", хотя все документы (.gob.gz) на месте.
//
// Этот инструмент дописывает отсутствующий metadata-файл, повторяя
// chromem-go persistMetadata: gob{ struct{Name; Metadata} } -> gzip.
// Без шифрования, compress=true — что соответствует go-magnetar:
// chromem.NewPersistentDB(dbPath, true).
//
// Формат выверен по исходникам chromem-go@v0.7.0:
//   - persistence.go: metadataFileName = "00000000", hash2hex = hex(sha256[:4])
//   - collection.go: persistMetadata → persistToFile(..., compress, "")
//   - db.go: чтение — readFromFile(fPath, &pc, "") cpc{Name, Metadata}

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

type collectionMeta struct {
	Name     string
	Metadata map[string]string
}

func hash2hex(name string) string {
	h := sha256.Sum256([]byte(name))
	return hex.EncodeToString(h[:4])
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: repair-chromem-meta <db-dir> <collection-name>\n")
		os.Exit(2)
	}
	dbDir, colName := os.Args[1], os.Args[2]

	dir := filepath.Join(dbDir, hash2hex(colName))
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "каталог коллекции не найден: %s\n", dir)
		os.Exit(1)
	}

	metaPath := filepath.Join(dir, "00000000.gob.gz")
	if _, err := os.Stat(metaPath); err == nil {
		fmt.Printf("ok: %s уже существует, ничего не делаю\n", metaPath)
		return
	}

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	enc := gob.NewEncoder(gzw)
	if err := enc.Encode(collectionMeta{Name: colName, Metadata: map[string]string{}}); err != nil {
		fmt.Fprintln(os.Stderr, "gob encode:", err)
		os.Exit(1)
	}
	if err := gzw.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "gzip close:", err)
		os.Exit(1)
	}

	if err := os.WriteFile(metaPath, buf.Bytes(), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("ok: восстановлен %s (collection %q)\n", metaPath, colName)
}
