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
- Do not process service messages such as: Balance status, confirmation requests with a one-time code, rejected transactions without debiting, information messages.
- If the image contains several separate SMS messages, output one object per message, in the order they appear, inside the JSON array.
- If the image contains only one SMS message, output an array with a single object.
- "expense" - a normal purchase or payment.
- "refund" - money returned for a previous purchase.
- "transfer" - a movement of money between the user's own accounts.
- "income" - money received that is not a refund or a transfer (e.g. salary, deposit).
- "unknown" - use only if the transaction type genuinely cannot be determined, but the SMS itself is readable.
- If the image cannot be read at all, or does not contain any bank transaction, output an array with a single object where "parse_failed" is true, "failure_reason" is filled, and the other fields have sensible empty defaults ("type": "unknown", "amount": 0).
- if a message doesn't clearly describe a transaction (amount debited/credited to a merchant/counterparty), it must not force it into "expense" - it should mark parse_failed: true or type: "unknown" rather than guess.
- If you cannot identify a clear debited/credited amount tied to a specific transaction, do not guess - set parse_failed true.
- Do not invent information that is not present or reasonably inferable from the image.
- Do not merge distinct SMS messages into one object, and do not drop any message present in the image.
- Respond with a raw JSON array only, matching the schema above exactly.`

// UserPrompt is the short instruction sent alongside each image.
const userPrompt = "Extract the expense data from every bank SMS message in this screenshot and return the JSON array as instructed."

// resolveSystemPrompt instructs the model how to match a parsed expense's topic against a short
// list of previously learned candidates from memory, per MEMORY-GUIDELINES.md.
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
