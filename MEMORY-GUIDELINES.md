# Expense Agent Memory Guidelines

Working with memory is only part of an agent's responsibilities.

## Purpose

In PostgreSQL, both training data and additional data provide context for the LLM. Training data represents a mapping of
possible input data and information on how this data should be enriched and recorded. Additional data represents static
information about card numbers and bank account mappings, expense categories, possible transaction characteristics, and
how to account for different currencies.

## Schema

Expected raw data from llm:

- type - TransactionType
- amount - float64
- currency - string
- bank_account - string
- date - string
- topic - string
- parse_failed - bool
- failure_reason - string, omitempty

Expected expenses data after enriching with additional data from memory:

- transaction_pk - string
- paired_transaction_fk - string
- name - string
- amount - float64
- note - string
- category_fk - string
- sub_category_fk - string
- wallet_fk - string
- date_created - int64
- date_time_modified - int64
- original_date_due - int64
- income - bool
- period_length - int64
- reoccurrence - int64
- end_date - int64
- upcoming_transaction_notification - bool
- type - int64
- paid - bool
- created_another_future_transaction - bool
- skip_paid - bool
- method_added - int64
- transaction_owner_email - string
- transaction_original_owner_email - string
- shared_key - string
- shared_old_key - string
- shared_status - int64
- shared_date_updated - int64
- shared_reference_budget_pk - string
- objective_fk - string
- objective_loan_fk - string
- budget_fks_exclude - string

Only records that match this structure can be entered into the database. You can use fixtures in the ./fixtures folder.

## Saving memory

- LLM extracts raw SMS message information from the screenshot according to the expected structure.
- The agent searches memory for the corresponding information based on the descriptions in the raw SMS data.
- The agent can also use the sqlite database to search for missing information in the "transactions", "wallets",
  "categories", "budgets", "associated_titles" tables. Read-only in this case.
- If information is found, it enriches the SMS data and proceeds to the next processing stage (no further memory
  considerations are involved).
- For messages for which information is missing or inaccurate, LLM is called again to generate a request for the
  required information for the user.
- If new data or new information from the user/LLM about matching transaction descriptions to the required category
  appears, the agent saves this information to memory.

## Memory usage in the process of enriching raw SMS

- Check for a reliable match between the raw SMS data and a specific transaction type. If there is a reliable match,
  skip the LLM entirely.
- If no confident match, query for top-N similar candidates and pass only those (not the full table) to the LLM as
  context.
- LLM context should stay small: extracted SMS text + top-N candidates. Don't dump entire tables.
- Any decision the LLM makes flows straight back into "saving memory" above — the loop never stops.

## Guardrails

- No LLM call for transactions already matched with high confidence.
- No full-table dumps into LLM prompts, ever.
- Create a request for additional information from the user if suitable information cannot be found for such data from
  the SMS.
