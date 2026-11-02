-- Список документов базы фильтруется по kb_id и сортируется по created_at.
CREATE INDEX idx_documents_kb_created
    ON documents (kb_id, created_at DESC);
