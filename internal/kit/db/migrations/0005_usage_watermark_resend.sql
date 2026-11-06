-- Watermark re-sends (contracts/metering.md, "Watermarks": "after correcting an hour that already
-- had its watermark, send that hour's watermark again"). usage_control_totals.changed_at is when
-- an hour's totals last changed (an insert, a correction, a withdrawal); usage_watermark_hours is
-- what each hour's watermark last said and when it was last checked. metering.Watermarks re-sends
-- an hour whose totals changed since and now differ from what was sent.
--
-- Rows from before this migration keep changed_at -infinity, so an upgrade re-sends nothing.
ALTER TABLE usage_control_totals ADD COLUMN changed_at timestamptz NOT NULL DEFAULT '-infinity';
CREATE INDEX usage_control_totals_changed_at_idx ON usage_control_totals (changed_at);

CREATE TABLE usage_watermark_hours (
  region           text NOT NULL,
  complete_through timestamptz NOT NULL,
  counts           jsonb NOT NULL,           -- the counts the last send carried
  sent_at          timestamptz NOT NULL,
  checked_at       timestamptz NOT NULL,     -- when the totals were last compared with them
  PRIMARY KEY (region, complete_through)
);
