import re

INJECTION = (
    re.compile(r"игнорир\w*.{0,40}(инструкц|правил|промпт|систем)", re.IGNORECASE),
    re.compile(r"забудь\w*.{0,40}(инструкц|правил|промпт|контекст)", re.IGNORECASE),
    re.compile(r"(ignore|disregard|forget)\s+(all\s+)?(previous|above|prior)", re.IGNORECASE),
    re.compile(r"(system|developer)\s*(prompt|message|instructions)", re.IGNORECASE),
    re.compile(r"jailbreak|dan\s+mode", re.IGNORECASE),
)

PERSONAL = (
    re.compile(r"(номер\s+карт|реквизит\w*\s+карт|cvv|cvc|пин-?код)", re.IGNORECASE),
    re.compile(r"(серия\s+и\s+номер\s+паспорт|паспортн\w+\s+данн)", re.IGNORECASE),
    re.compile(r"(снилс|инн\s+физлица)", re.IGNORECASE),
)

_CARD = re.compile(r"\b(?:\d[ -]?){13,19}\b")
_DIGITS = re.compile(r"\D")


def _luhn(raw: str) -> bool:
    digits = [int(char) for char in _DIGITS.sub("", raw)]
    if len(digits) < 13:
        return False
    total = 0
    for index, digit in enumerate(reversed(digits)):
        if index % 2:
            digit *= 2
            if digit > 9:
                digit -= 9
        total += digit
    return total % 10 == 0


def blocked_reason(text: str) -> str | None:
    """Вернуть метку отказа или None.

    Parameters
    ----------
    text : str
        Вопрос пользователя.

    Returns
    -------
    str | None
        ``prompt_injection``, ``personal_data`` или None.
    """
    if any(pattern.search(text) for pattern in INJECTION):
        return "prompt_injection"
    has_card = any(_luhn(match.group(0)) for match in _CARD.finditer(text))
    if has_card or any(pattern.search(text) for pattern in PERSONAL):
        return "personal_data"
    return None
