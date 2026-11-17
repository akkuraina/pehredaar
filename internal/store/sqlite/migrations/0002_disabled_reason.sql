-- Why a source stopped being polled: '' (enabled), 'removed' (gone from
-- sources.yaml), 'gone' (404/410 repeatedly), 'needs-ocr' (scanned PDF),
-- 'retired' (discovered PDF past its watch period).

ALTER TABLE sources ADD COLUMN disabled_reason TEXT NOT NULL DEFAULT ''
    CHECK (disabled_reason IN ('', 'removed', 'gone', 'needs-ocr', 'retired'));

-- Sources already disabled before this migration were removed from the yaml.
UPDATE sources SET disabled_reason = 'removed' WHERE enabled = 0;
