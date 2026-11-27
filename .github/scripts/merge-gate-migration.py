#!/usr/bin/env python3
"""Inject base-owned migration authority only into the isolated ordinary migration unit."""
from contextlib import contextmanager
import json
import os
from pathlib import Path
import re

NAMES = ("TestMergeGateAllMigrationPrefixesUpgradeReopenReplay",
         "TestMergeGatePublishedMigrationAuthority", "TestMergeGatePublishedMigrationAuthorityRejectsMutation")
TEMPLATE = r'''package store

import (
    "crypto/sha256"
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
    "regexp"
    "testing"
)

var mergeGatePublishedDigests = []string{
__DIGESTS__
}

func mergeGateCheckPublishedSQL(sql []string) error {
    if len(sql) < len(mergeGatePublishedDigests) {
        return fmt.Errorf("published migration prefix shortened: %d", len(sql))
    }
    for i, want := range mergeGatePublishedDigests {
        if got := fmt.Sprintf("%x", sha256.Sum256([]byte(sql[i]))); got != want {
            return fmt.Errorf("published migration v%d changed: %s != %s", i+1, got, want)
        }
    }
    return nil
}

func mergeGateCheckCandidateRegistry(sql []string) error {
    if err := mergeGateCheckPublishedSQL(sql); err != nil {
        return err
    }
    data, err := os.ReadFile(filepath.Join("..", "..", ".github", "scripts", "merge-gate-migrations.json"))
    if err != nil {
        return err
    }
    var registry struct {
        Base string `json:"base"`
        VersionCount int `json:"version_count"`
        SHA256 []string `json:"sha256"`
    }
    if err := json.Unmarshal(data, &registry); err != nil {
        return err
    }
    if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(registry.Base) || registry.VersionCount == 0 ||
        registry.VersionCount != len(sql) || len(registry.SHA256) != len(sql) {
        return fmt.Errorf("candidate registry must cover the complete runtime registry: count=%d digests=%d runtime=%d",
            registry.VersionCount, len(registry.SHA256), len(sql))
    }
    for i, want := range registry.SHA256 {
        if got := fmt.Sprintf("%x", sha256.Sum256([]byte(sql[i]))); got != want {
            return fmt.Errorf("candidate registry v%d digest/order mismatch: %s != %s", i+1, got, want)
        }
    }
    return nil
}

func TestMergeGatePublishedMigrationAuthority(t *testing.T) {
    if err := mergeGateCheckCandidateRegistry(migrations); err != nil {
        t.Fatal(err)
    }
}

func TestMergeGatePublishedMigrationAuthorityRejectsMutation(t *testing.T) {
    edited := append([]string(nil), migrations...)
    edited[0] += "\n-- a candidate cannot rewrite a published migration\n"
    if err := mergeGateCheckPublishedSQL(edited); err == nil {
        t.Fatal("modified published SQL was accepted")
    }
}
'''


def source(authority):
    value = json.loads(Path(authority).read_text(encoding="utf-8"))
    digests = value["sha256"]
    if (not isinstance(value.get("base"), str) or not re.fullmatch(r"[0-9a-f]{40}", value["base"]) or
            type(value.get("version_count")) is not int or not isinstance(digests, list) or
            value["version_count"] != len(digests) or not digests or
            not all(isinstance(d, str) and re.fullmatch(r"[0-9a-f]{64}", d) for d in digests)):
        raise ValueError("已发布迁移的独立摘要登记非法")
    return TEMPLATE.replace("__DIGESTS__", "\n".join('    "' + digest + '",' for digest in digests))


@contextmanager
def generated(root, authority):
    store = Path(root) / "internal/store"
    upgrade = Path(__file__).with_name("merge-gate-migration_test.go.template").read_text(encoding="utf-8")
    created = []
    try:
        for name, data in (("merge_gate_authority_generated_test.go", source(authority)),
                           ("merge_gate_migration_generated_test.go", upgrade)):
            path = store / name
            # Never overwrite candidate files, including a candidate's same-name symlink.
            with path.open("x", encoding="utf-8") as output:
                created.append((path, os.fstat(output.fileno()).st_ino))
                output.write(data)
        yield []
    finally:
        for path, inode in reversed(created):
            if path.is_file() and not path.is_symlink() and path.stat().st_ino == inode:
                path.unlink()
