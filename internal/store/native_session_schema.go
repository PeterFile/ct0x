package store

// V23 appends to the immutable F5 migrations through V22. Old Turns receive
// only unknown/no-reference barriers, never inferred grants or replayed work.
const nativeRunBindingsSchemaV23 = `
CREATE UNIQUE INDEX turns_native_binding_identity ON turns(id,group_id,member_id,conversation_id);
CREATE TABLE native_run_bindings (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    turn_id TEXT NOT NULL UNIQUE,
    group_id TEXT NOT NULL,
    member_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    purpose TEXT NOT NULL CHECK(purpose IN ('unknown','generic','chat_intake/v1','coordinator','worker')),
    binding_version INTEGER NOT NULL CHECK(typeof(binding_version)='integer' AND binding_version IN (0,1)),
    claim_digest BLOB CHECK(claim_digest IS NULL OR (typeof(claim_digest)='blob' AND length(claim_digest)=32)),
    binding_json TEXT CHECK(binding_json IS NULL OR (typeof(binding_json)='text' AND length(CAST(binding_json AS BLOB))<=16384 AND json_valid(binding_json))),
    profile_sha256 TEXT CHECK(profile_sha256 IS NULL OR (length(profile_sha256)=64 AND profile_sha256 NOT GLOB '*[^0-9a-f]*')),
    native_ref_json TEXT CHECK(native_ref_json IS NULL OR (typeof(native_ref_json)='text' AND length(CAST(native_ref_json AS BLOB))<=32768 AND json_valid(native_ref_json))),
    resumed_from_turn_id TEXT,
    UNIQUE(turn_id,group_id,member_id,conversation_id,purpose),
    FOREIGN KEY(turn_id,group_id,member_id,conversation_id) REFERENCES turns(id,group_id,member_id,conversation_id),
    FOREIGN KEY(resumed_from_turn_id,group_id,member_id,conversation_id,purpose)
        REFERENCES native_run_bindings(turn_id,group_id,member_id,conversation_id,purpose),
    CHECK((binding_version=0 AND binding_json IS NULL AND profile_sha256 IS NULL AND native_ref_json IS NULL AND resumed_from_turn_id IS NULL)
       OR (binding_version=1 AND binding_json IS NOT NULL AND profile_sha256 IS NOT NULL)),
    CHECK(native_ref_json IS NULL OR purpose<>'unknown'),
    CHECK(resumed_from_turn_id IS NULL OR resumed_from_turn_id<>turn_id)
);
CREATE INDEX native_run_bindings_latest ON native_run_bindings(group_id,member_id,conversation_id,seq DESC);
CREATE TRIGGER native_run_bindings_insert BEFORE INSERT ON native_run_bindings
WHEN EXISTS(SELECT 1 FROM native_run_bindings WHERE turn_id=NEW.turn_id OR seq=NEW.seq)
BEGIN SELECT RAISE(ABORT, '原生会话关联不可替换'); END;
CREATE TRIGGER native_run_bindings_no_update BEFORE UPDATE ON native_run_bindings
BEGIN SELECT RAISE(ABORT, '原生会话关联不可修改'); END;
CREATE TRIGGER native_run_bindings_no_delete BEFORE DELETE ON native_run_bindings
BEGIN SELECT RAISE(ABORT, '原生会话关联不可删除'); END;
INSERT INTO native_run_bindings(turn_id,group_id,member_id,conversation_id,purpose,binding_version)
    SELECT id,group_id,member_id,conversation_id,'unknown',0 FROM turns ORDER BY created_at,id;
`
