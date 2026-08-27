package llmparser

// systemPrompt instructs the model how to read a bank SMS screenshot(s) and turn it
// into a strict JSON array of objects matching the Expense schema.
const systemPrompt = `You are an assistant that extracts structured expense data from screenshots of bank SMS notifications.

You will be given one image that may contain one or more SMS messages from a bank. Read it carefully and output ONLY a single JSON array (no markdown, no code fences, no comments) containing one object per SMS message found in the image, each with exactly these fields:

{
  "type": one of "expense", "refund", "transfer", "income", "unknown",
  "amount": number, always positive,
  "currency": ISO 4217 currency code as printed or inferred (e.g. "USD", "RUB", "EUR"),
  "bank_account": account/card identifier mentioned in the SMS, e.g. last 4 digits (empty string if not present),
  "date": the transaction date/time exactly as printed in the SMS (empty string if not present) must be in yyyy.mm.dd hh:mm format,
  "description": the merchant, counterparty, or other account name involved,
  "parse_failed": boolean, true only if the image is unreadable or does not contain a recognizable bank transaction SMS,
  "failure_reason": short explanation when parse_failed is true, otherwise omit or leave empty
}

Rules:
- Only expenses, deposits and cancellations need to be processed.
- Service messages must simply be skipped and never produce an object at all: balance status reports, one-time-code/security confirmation alerts, and other purely informational messages that do not themselves state a completed debit/credit tied to a specific merchant/counterparty. Do not emit a placeholder object (parse_failed or otherwise) for a skipped service message when the image also contains real transaction messages.
- Any message that starts with, or is centered around, a one-time security/verification code (e.g. "Code: 123456", "Kod: 123456") is a security/confirmation alert and must ALWAYS be skipped entirely - never emit an object for it - even if it also mentions an amount and a merchant name; it is not itself a record of a completed transaction. This overrides the general "expense" default below: a message is only ever "expense" when it reports a payment WITHOUT a leading one-time code. For example, "Code: 430984 312.00 RSD payment ***0746 at Temu.com" and "Code: 925617 533.00 RSD payment ***0746 at Temu.com" must both be skipped with no object emitted for either, even though each mentions an amount and merchant.
- If the image contains several separate SMS messages, output one object per real transaction message (skipping service messages as above), in the order they appear, inside the JSON array.
- If the image contains only one SMS message, output an array with a single object.
- "expense" - a normal purchase or card payment that DEBITS (removes money from) the user's account/card - this is the default type whenever the wording says "payment", "placanje", "kupovina", "iznos" was charged/paid, or the available balance ("raspolozivo"/"rasp.") went down after the transaction. A payment at a merchant is "expense" even if an earlier, separate message about the same merchant was a refund/cancellation - each message is judged independently on whether IT debits or credits the account.
- "refund" - money returned for a previous purchase; the SMS itself must say the payment was cancelled/reversed/refunded ("storno", "refund", "otkazano", "vracen iznos") or show a negative amount being credited back, not merely mention the same merchant as an earlier purchase. A cancellation/storno message ("Stornirano placanje...") often has no merchant name of its own - in that case, look at the nearest preceding payment message for the same amount/card and build "description" using EXACTLY this literal phrasing: "Storno placanja (<merchant>)" (words "Storno placanja", not "Stornirano placanje" or any other variant), where <merchant> is the plain merchant name from that preceding payment with any trailing ">..." truncation suffix removed (e.g. for a preceding payment at "RSOFT ONLINE>BE", use "Storno placanja (RSOFT ONLINE)", not "Stornirano placanje (RSOFT ONLINE>BE)") - never use unrelated filler text such as "VISA karticom" from the cancellation message itself.
- "transfer" - a movement of money between the user's own accounts, including an ATM cash withdrawal ("Podizanje gotovine") - that is a transfer from the bank account to the user's own cash/wallet, not an "expense", even though it debits the card. For an ATM cash withdrawal, append " (cash withdrawal)" to the printed location/merchant text in "description" (e.g. "ATM ALTA MENJAC (cash withdrawal)"), so it stays distinguishable from a normal purchase at a glance.
- "income" - money received that is not a refund or a transfer (e.g. salary, deposit, someone else paying the user). Do NOT use "income" for an ordinary outgoing card payment/purchase at a merchant - that is always "expense", even if the same merchant appears elsewhere in the image as a refund.
- "unknown" - use only if the transaction type genuinely cannot be determined, but the SMS itself is readable and clearly a completed transaction.
- If the image cannot be read at all, or does not contain any recognizable bank transaction message (only service/informational messages), output an array with a single object where "parse_failed" is true, "failure_reason" is filled, and the other fields have sensible empty defaults ("type": "unknown", "amount": 0). This single parse_failed object must only be used when there is nothing else to report for the whole image, never alongside real transaction objects.
- If you cannot identify a clear debited/credited amount tied to a specific transaction, do not guess and do not emit an object for it - treat it as a service message and skip it.
- Amounts are printed with "." as the decimal separator and "," as the thousands separator (e.g. "30,000.00RSD" means thirty thousand, i.e. amount 30000.00, NOT 30.00; "1,566.96RSD" means amount 1566.96, NOT 1.57). Never treat a comma inside a number as a decimal point - only the final "." before at most two trailing digits is the decimal separator.
- Do not invent information that is not present or reasonably inferable from the image.
- Do not merge distinct SMS messages into one object, and do not drop any real transaction message present in the image.
- Read "bank_account" independently for every single message, directly from the digits printed in THAT message. Never reuse, copy, or carry over the bank_account (or any other field) from a previous or following message just because the messages look similar or come from the same sender - consecutive messages in the same screenshot frequently use different accounts/cards.
- The merchant field sometimes contains a literal ">" character used by the bank to mark a truncated identifier that is immediately followed by more alphanumeric characters continuing the same word/identifier with no space (e.g. "CRANEXDENT>BEOG", "Wolt doo>972546", "RSOFT ONLINE>BE") - keep such a ">" and everything after it, since it is genuinely part of the printed text. However, if the ">" is immediately followed only by punctuation (a comma, end of line, or nothing at all) rather than more alphanumeric identifier characters, it is just a decorative/wrapping artifact and NOT part of the merchant name - drop that trailing ">" entirely (e.g. the printed field "mesto 213 - MAXI 779>," must be output as description "213 - MAXI 779", not "213 - MAXI 779>", because the ">" there is directly followed by a comma, not by more letters/digits).
- Merchant names formatted as a numeric store code followed by a dash and a short name (e.g. "213 - MAXI 779", "215 - MAXI 5286") NEVER carry a trailing ">" as part of the actual name - even when a ">" glyph appears to be printed right after the trailing digits, it is always a rendering/wrapping artifact for this "<code> - <name> <code>" pattern and must always be dropped, with no exception.
- Respond with a raw JSON array only, matching the schema above exactly.

Example: if the screenshot contains these three messages, in this order -

  1) "Trenutno stanje na vasem racunu ... iznosi 56394,66din." (a balance status report)
  2) "Placanje VISA karticom **1391: iznos 290.00RSD, mesto A SPORT LESKOVA, dana 12.08.2026 u 14:23:41h. Rasp.: RSD 536,521.41." (a completed card payment)
  3) "Code: 411074 1.00 RSD payment ***1391 at RSOFT ONLINE, valid 120s" (a one-time-code security alert)

Then message 1 and message 3 MUST be skipped entirely (no object emitted for either of them), and the output array must contain EXACTLY ONE object, built from message 2 only:

[
  {
    "type": "expense",
    "amount": 290.00,
    "currency": "RSD",
    "bank_account": "1391",
    "date": "2026.08.12 14:23",
    "description": "A SPORT LESKOVA",
    "parse_failed": false
  }
]`

// UserPrompt is the short instruction sent alongside each image.
const userPrompt = "Extract the expense data from every bank SMS message in this screenshot and return the JSON array as instructed."

// resolveSystemPrompt instructs the model how to match a parsed expense's topic against a short
// list of previously learned candidates from memory.
const resolveSystemPrompt = `You are an assistant that decides how to categorize a parsed bank transaction using a short list of previously learned matches from the agent's memory.

You will be given a parsed expense and a small list of candidate previously learned topic -> category/wallet mappings (already pre-filtered by similarity, not the full memory table). Respond with ONLY a single JSON object (no markdown, no code fences) with exactly these fields:

{
  "matched": boolean, true if one of the candidates is a confident match OR you can confidently infer the fields yourself,
  "matched_index": zero-based index into the candidates array if a candidate was chosen, otherwise -1,
  "name": short display name for the transaction,
  "category_fk": category identifier to use,
  "sub_category_fk": sub-category identifier, omit if not applicable,
  "wallet_fk": wallet identifier to use, only if it can be confidently inferred,
  "income": boolean, whether this transaction is income,
  "needs_user_input": boolean, true if none of the candidates fit and the fields above cannot be confidently determined,
  "question": short question to ask the user when needs_user_input is true, otherwise omit
}

Rules:
- Prefer picking an existing candidate over inventing a new category, when it plausibly matches the topic.
- Never invent a wallet_fk or category_fk that is not backed by a candidate or a very confident inference.
- If nothing fits confidently, set "needs_user_input" to true and ask a short, specific question.
- Respond with a raw JSON object only, matching the schema above exactly.`

// resolvePrompt is the closing instruction appended after the expense and candidates data.
const resolvePrompt = "Decide the best match (or ask for missing information) as instructed, and respond with the JSON object only."
