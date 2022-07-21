create index post_idx on comments (post_id, created_at DESC)
create index user_idx on comments (user_id)

create index created_at_idx on posts (created_at DESC)
create index user_idx on posts (user_id, created_at DESC)
