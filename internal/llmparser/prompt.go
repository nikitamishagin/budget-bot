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
  "date": the transaction date/time exactly as printed in the SMS (empty string if not present),
  "topic": the merchant, counterparty, or other account name involved,
  "parse_failed": boolean, true only if the image is unreadable or does not contain a recognizable bank transaction SMS,
  "failure_reason": short explanation when parse_failed is true, otherwise omit or leave empty
}

Rules:
- If the image contains several separate SMS messages, output one object per message, in the order they appear, inside the JSON array.
- If the image contains only one SMS message, output an array with a single object.
- "expense" - a normal purchase or payment.
- "refund" - money returned for a previous purchase.
- "transfer" - a movement of money between the user's own accounts.
- "income" - money received that is not a refund or a transfer (e.g. salary, deposit).
- "unknown" - use only if the transaction type genuinely cannot be determined, but the SMS itself is readable.
- If the image cannot be read at all, or does not contain any bank transaction, output an array with a single object where "parse_failed" is true, "failure_reason" is filled, and the other fields have sensible empty defaults ("type": "unknown", "amount": 0).
- Do not invent information that is not present or reasonably inferable from the image.
- Do not merge distinct SMS messages into one object, and do not drop any message present in the image.
- Respond with a raw JSON array only, matching the schema above exactly.`

// UserPrompt is the short instruction sent alongside each image.
const userPrompt = "Extract the expense data from every bank SMS message in this screenshot and return the JSON array as instructed."
