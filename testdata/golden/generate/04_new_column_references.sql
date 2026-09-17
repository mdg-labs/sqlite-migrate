ALTER TABLE orders ADD COLUMN user_id integer REFERENCES users (id);
