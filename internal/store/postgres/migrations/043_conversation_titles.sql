-- Member-set conversation titles (PATCH /api/v1/conversations/{id}): an
-- empty title keeps the service's derived preview from the first visible
-- user message.
ALTER TABLE conversations
    ADD COLUMN title TEXT NOT NULL DEFAULT '';
