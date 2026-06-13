package gateway

type conflictError struct{ message string }

func (e *conflictError) Error() string { return e.message }
