#!/usr/bin/env python3
"""Generate independent old-prefix authority into an unprivileged candidate test build."""
from contextlib import contextmanager
import json
from pathlib import Path
import re
import tempfile

TEMPLATE = r'''package store

import (
    "crypto/sha256"
    "fmt"
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

func TestMergeGatePublishedMigrationAuthority(t *testing.T) {
    if err := mergeGateCheckPublishedSQL(migrations); err != nil {
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
    if (not re.fullmatch(r"[0-9a-f]{40}", value["base"]) or value["version_count"] != len(digests) or
            not digests or not all(isinstance(d, str) and re.fullmatch(r"[0-9a-f]{64}", d) for d in digests)):
        raise ValueError("已发布迁移的独立摘要登记非法")
    return TEMPLATE.replace("__DIGESTS__", "\n".join('    "' + digest + '",' for digest in digests))


@contextmanager
def generated(root, authority):
    path = Path(root) / "internal/store/merge_gate_authority_generated_test.go"
    # Never overwrite candidate files; a collision is a hard error.
    with path.open("x", encoding="utf-8") as output:
        output.write(source(authority))
    inode = path.stat().st_ino
    try:
        with tempfile.TemporaryDirectory(prefix="merge-authority-") as temporary:
            trusted = Path(authority).resolve().parents[2] / "internal/store/merge_gate_migration_test.go"
            if not trusted.is_file():
                raise ValueError("缺少受信历史前缀升级测试模板")
            overlay = Path(temporary) / "overlay.json"
            overlay.write_text(json.dumps({"Replace": {
                str((Path(root) / "internal/store/merge_gate_migration_test.go").resolve()): str(trusted)
            }}), encoding="utf-8")
            yield ["-overlay=" + str(overlay)]
    finally:
        if path.is_file() and not path.is_symlink() and path.stat().st_ino == inode:
            path.unlink()
