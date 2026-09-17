ALTER TABLE orders ADD COLUMN user_id integer NOT NULL DEFAULT 0 REFERENCES users (id);
