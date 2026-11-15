"""Errors api returns. `code` is one of contract/vocabulary.json errorCodes."""


class ApiError(Exception):
    def __init__(self, status: int, code: str, message: str):
        super().__init__(message)
        self.status = status
        self.code = code
        self.message = message


def forbidden(message: str = "you are not allowed to do this") -> ApiError:
    return ApiError(403, "forbidden", message)
