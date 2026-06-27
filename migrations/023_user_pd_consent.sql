-- +goose Up
-- +goose StatementBegin

-- pd_consent_at — когда пользователь подтвердил согласие на обработку ПД (152-ФЗ)
-- явным действием в боте (кнопка «Принимаю»). NULL = согласие ещё не получено →
-- бот показывает экран согласия и не обрабатывает запросы до подтверждения.
-- Текст согласия: tryberry.ru/#consent, политика: tryberry.ru/#privacy.
ALTER TABLE users ADD COLUMN IF NOT EXISTS pd_consent_at TIMESTAMPTZ;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE users DROP COLUMN IF EXISTS pd_consent_at;
-- +goose StatementEnd
