-- Write your migrate up statements here
CREATE TABLE IF NOT EXISTS answers (
    id uuid PRIMARY KEY NOT NULL DEFAULT gen_random_uuid(),
    message_id uuid NOT NULL,
    answer text NOT NULL,
    reaction_count bigint DEFAULT 0,
    created_at timestamp DEFAULT now(),
    FOREIGN KEY (message_id) REFERENCES messages (
        id
    ) ON DELETE CASCADE ON UPDATE CASCADE
);
---- create above / drop below ----
DROP TABLE IF EXISTS answers;
-- Write your migrate down statements here. If this migration is irreversible
-- Then delete the separator line above.
