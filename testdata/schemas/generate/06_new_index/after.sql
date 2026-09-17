CREATE TABLE orders (
    id INTEGER PRIMARY KEY,
    user_id INTEGER
) STRICT;

CREATE INDEX idx_orders_user_id ON orders(user_id);
