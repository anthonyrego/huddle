-- Runs once, when the dev Postgres volume is first created: a second
-- database for the store's tests, so they never touch the dev data.
CREATE DATABASE huddle_test OWNER huddle;
