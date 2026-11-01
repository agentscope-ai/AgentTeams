-- 0007: storage_refs - the single reference source for object-store
-- GC: a blob is deleted only at refcount 0 (W1).
CREATE TABLE storage_refs (
  object_key VARCHAR(1024) NOT NULL,
  sha256 CHAR(64) NOT NULL,
  size_bytes BIGINT NOT NULL,
  owner_type VARCHAR(16) NOT NULL,          -- memory|knowledge|export|backup
  owner_id VARCHAR(512) NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (object_key, owner_type, owner_id),
  KEY idx_storage_owner (owner_type, owner_id)
);
