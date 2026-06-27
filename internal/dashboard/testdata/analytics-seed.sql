-- Deterministic analytics fixtures shared by the Node golden generator
-- (gen-analytics.js) and the Go parity test. FIXED_NOW = 2026-06-08T00:00:00Z.
-- Timestamps use the SQLite 'YYYY-MM-DD HH:MM:SS' (UTC) form, like datetime('now').

INSERT INTO jobs (id, title, company, url, location, score, status, stage, applied_at, rejected_from_stage, posted_at, created_at) VALUES
  ('j1', 'Platform Engineer', 'Acme', 'https://boards.greenhouse.io/acme/jobs/1', 'Remote', 9, 'applied', 'phone_screen', '2026-06-01 00:00:00', NULL, '2026-05-30', '2026-05-30 00:00:00'),
  ('j2', 'SRE', 'Globex', 'https://jobs.lever.co/globex/2', 'Austin', 6, 'rejected', 'rejected', '2026-05-15 00:00:00', 'interview', '2026-05-10', '2026-05-10 00:00:00'),
  ('j3', 'DevOps Engineer', 'Initech', 'https://jobs.ashbyhq.com/initech/3', 'Remote', 7, 'applied', NULL, '2026-05-01 00:00:00', NULL, '2026-04-28', '2026-04-28 00:00:00'),
  ('j4', 'Backend Engineer', 'Umbrella', 'https://umbrella.wd1.myworkdayjobs.com/job/4', 'Seattle', 9, 'pending', NULL, NULL, NULL, '2026-06-05', '2026-06-05 00:00:00'),
  ('j5', 'Junior Dev', 'Hooli', 'https://boards.greenhouse.io/hooli/jobs/5', 'Remote', 3, 'applied', NULL, '2026-06-02 00:00:00', NULL, '2026-05-31', '2026-05-31 00:00:00'),
  ('j6', 'Old Interview', 'Initrode', 'https://jobs.lever.co/initrode/6', 'Remote', 8, 'closed', 'closed', NULL, NULL, '2026-03-28', '2026-03-28 00:00:00'),
  ('j7', 'Pending Applied', 'Stark', 'https://jobs.ashbyhq.com/stark/7', 'Remote', 5, 'pending', NULL, '2026-05-28 00:00:00', NULL, '2026-05-25', '2026-05-25 00:00:00'),
  ('j8', 'Rejected From Interview', 'Wayne', 'https://wayne.wd1.myworkdayjobs.com/job/8', 'Remote', 4, 'rejected', 'rejected', NULL, 'interview', '2026-04-02', '2026-04-02 00:00:00'),
  ('j9', 'Applied Event Only', 'Wonka', 'https://boards.greenhouse.io/wonka/jobs/9', 'Remote', 4, 'archived', NULL, NULL, NULL, '2026-05-20', '2026-05-20 00:00:00'),
  ('j10', 'Rejected From Event', 'Soylent', 'https://example.com/jobs/10', 'Remote', 10, 'rejected', 'rejected', NULL, NULL, '2026-04-05', '2026-04-05 00:00:00');

INSERT INTO events (job_id, event_type, from_value, to_value, created_at) VALUES
  ('j1', 'stage_change', NULL, 'applied', '2026-06-01 00:00:00'),
  ('j1', 'stage_change', 'applied', 'phone_screen', '2026-06-03 00:00:00'),
  ('j2', 'stage_change', NULL, 'applied', '2026-05-15 00:00:00'),
  ('j2', 'stage_change', 'applied', 'interview', '2026-05-20 00:00:00'),
  ('j2', 'stage_change', 'interview', 'rejected', '2026-05-25 00:00:00'),
  ('j3', 'stage_change', NULL, 'applied', '2026-05-01 00:00:00'),
  ('j5', 'stage_change', NULL, 'applied', '2026-06-02 00:00:00'),
  ('j6', 'stage_change', NULL, 'interview', '2026-04-01 00:00:00'),
  ('j9', 'stage_change', NULL, 'applied', '2026-05-21 00:00:00'),
  ('j10', 'stage_change', 'interview', 'rejected', '2026-04-12 00:00:00');
