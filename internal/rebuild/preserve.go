package rebuild

// Preserve recreates every index, trigger, view, and foreign key that
// referenced a table before its rebuild, once the rebuilt table is in
// place under its original name.
