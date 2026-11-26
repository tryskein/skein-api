# --- classify: the answer of a channel -> one outcome ("Delivery outcomes") --
# One rule for every record:
#   1. a record has a closed table of answer signs, each mapped to exactly
#      one outcome; an answer that matches no row, or more than one, a
#      corrupt or unfamiliar answer (a new version of the channel) is
#      "unknown";
#   2. an answer is "refused before send" only when the text was not handed
#      to the channel in this attempt and every earlier attempt of this
#      delivery is known as refused before send; otherwise "unknown";
#   3. a delivery is repeated only after a refusal by item 2 (ledger.py and
#      deliver.py keep to it);
#   4. a wait after the send (idle, a receipt) that runs out is "unknown",
#      not a refusal: a call that times out is "unknown".

# The outcomes a channel table may name (registry.go#OutcomeRefused and the
# rest).
CHANNEL_OUTCOMES = frozenset((OUTCOME_REFUSED, OUTCOME_QUEUED, OUTCOME_INPUT_WRITTEN, OUTCOME_INPUT_NO_RECEIPT,
                              OUTCOME_TURN_STARTED, OUTCOME_UNKNOWN, "addressee-changed",
                              OUTCOME_ADDRESSEE_GONE))
# Outcomes after which the event reached the addressee's queue or input.
DELIVERED_OUTCOMES = frozenset((OUTCOME_QUEUED, OUTCOME_INPUT_WRITTEN, OUTCOME_INPUT_NO_RECEIPT,
                                OUTCOME_TURN_STARTED))


def answer_sign(exit_code=None, doc=None, http_status=None, timed_out=False):
    """The sign of an answer as a journal line names it: the exit code, the
    HTTP status, or the timeout."""
    if timed_out:
        return {"timeout": True}
    if http_status is not None:
        return {"httpStatus": http_status}
    return {"exitCode": exit_code}


def classify(rows, exit_code=None, doc=None, http_status=None, timed_out=False,
             written=False, earlier_refused=True):
    """(outcome, sign) of one answer by the closed table rows. written: the
    text was handed to the channel in this attempt (a step that may have
    written it ran); earlier_refused: every earlier attempt of this
    delivery was refused before send."""
    if timed_out:
        return OUTCOME_UNKNOWN, answer_sign(timed_out=True)
    matched = [r for r in rows or [] if sign_matches(r.get("match") or {}, exit_code=exit_code,
                                                      doc=doc, http_status=http_status)]
    if len(matched) != 1:
        return OUTCOME_UNKNOWN, answer_sign(exit_code, doc, http_status)
    outcome = matched[0].get("outcome")
    if outcome not in CHANNEL_OUTCOMES:
        return OUTCOME_UNKNOWN, matched[0].get("match")
    if outcome == OUTCOME_REFUSED and (written or not earlier_refused):
        return OUTCOME_UNKNOWN, matched[0].get("match")
    return outcome, matched[0].get("match")


def classify_result(rows, result, written=False, earlier_refused=True):
    """classify for a CallResult of the argv executor."""
    return classify(rows, exit_code=result.exit_code, doc=result.json, timed_out=result.timed_out,
                    written=written, earlier_refused=earlier_refused)
