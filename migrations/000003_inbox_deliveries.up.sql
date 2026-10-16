ALTER TABLE wagering.inbox ADD COLUMN deliveries integer NOT NULL DEFAULT 1 CHECK (deliveries > 0);
