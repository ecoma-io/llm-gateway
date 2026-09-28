<script setup lang="ts">
// Payments: funding this account by buying one of the offers this deployment
// publishes, and reading what became of each attempt.
//
// **The browser is not a financial boundary, and this screen is where that has
// to be visible rather than merely true.** A payment's status is written ONLY
// by a signature-verified webhook from the provider's own servers. The money
// moves as a domestic bank transfer to an account the provider holds for the
// payment; this application never sees an instrument of any kind, there is no
// card number and no bank credential anywhere in the contract, and so there is
// no form here and nothing on this screen that collects one. The customer is
// handed the account to pay into and stays on this page.
//
// **Nothing here navigates, and that is the shape of the whole change.** The
// previous provider's instrument was a hosted page, so the console's job after
// opening a payment was to send the browser to it. A bank transfer has no page
// to visit, so the instructions are rendered in place and the browser never
// leaves. The one thing that redirect said silently — that the customer may
// stop looking at this screen — is now said in words beside the instructions.
//
// A refresh is a status RE-READ and nothing else. It cannot mark anything paid:
// the customer coming back, or saying they have sent the money, is not evidence
// that money moved, and a console that treated it as evidence would be deciding
// the instant a balance changed. So the copy says so in as many words — while a
// payment is in a state the provider has not confirmed, the screen says it is
// waiting for the provider and that refreshing is how the outcome is learned.
// Nothing on this page ever derives a funded state from a browser event.
//
// **The offer carries the price and the request must not.** A top-up names an
// offer and nothing else: `CreatePaymentIntentRequest` has no amount and no
// currency, deliberately, because a client that could set a price could charge
// itself one minor unit. This screen reads the prices and sends an offer id,
// which is the whole reason the chooser is built from `GET /top-up-offers`
// rather than from a number a page holds.
//
// **The idempotency key is not a nonce.** `modules/payments/idempotency.ts`
// holds the rule in full; the short version is that the key outlives a single
// attempt, so a retry after a dropped response or a provider outage repeats the
// same request and converges on the same payment, while a SECOND deliberate
// top-up — or the same offer chosen after the first attempt finished — gets a
// new key and produces a new payment. Reusing a key across two deliberate
// top-ups would swallow the second one, and regenerating one on a retry would
// charge the customer twice for one act.
import { Button, Card, LoadingState, PageHeader, Stack } from "@ecoma-io/loom";
import { computed, onMounted, ref } from "vue";

import DataTable from "@/components/DataTable.vue";
import { createPaymentForOffer, fetchPaymentIntents, fetchTopUpOffers } from "@/lib/api";
import { usePagedList } from "@/lib/paged-list";
import { useResource } from "@/lib/resource";
import type { DataTableColumn } from "@/components/data-table";
import type { Failure } from "@/lib/api";
import FailureView from "@/modules/failure/FailureView.vue";
import { beginTopUp, retireTopUp, type TopUpAttempt } from "@/modules/payments/idempotency";
import {
  showTransferInstructions,
  type TransferInstructions,
} from "@/modules/payments/instructions";
import TransferInstructionsPanel from "@/modules/payments/TransferInstructionsPanel.vue";
import { formatPrice, formatPriceAmount } from "@/modules/payments/offer-price";
import InstantCell from "@/modules/status/InstantCell.vue";
import StatusBadge from "@/modules/status/StatusBadge.vue";
import {
  PAYMENT_AWAITING_PROVIDER_STATES,
  PAYMENT_STATE_EXPLANATION,
  PAYMENT_STATE_PRESENTATION,
} from "@/modules/status/presentation";
import type {
  ListPaymentIntentsData,
  PaymentIntent,
  PaymentIntentPage,
  PaymentIntentState,
  TopUpOffer,
  TopUpOfferList,
} from "@ecoma-io/llm-gateway-console-api-client";

/** The offers this deployment publishes. Not paged: a price list has no position to name. */
const offers = useResource<TopUpOfferList>(() => fetchTopUpOffers());

/**
 * The account's payments, newest first.
 *
 * The cursor rides the contract's own key, `after`. This route carries exactly
 * one list — the offer read above is not paged and is not in the URL at all —
 * so there is no neighbouring cursor for it to collide with, and the contract's
 * own name is the honest one (`CommercePage`'s note on `entitlements_after`
 * explains what a second list on one route would cost).
 */
const payments = usePagedList<PaymentIntentPage, ListPaymentIntentsData["query"]>({
  read: (query) => fetchPaymentIntents({ query }),
  shape: { filters: [], cursor: "after" },
  vocabulary: {},
});

/**
 * The one attempt the customer has committed to whose answer is still open,
 * held in memory for the life of this screen and nowhere else.
 *
 * It is the idempotency key's carrier and the reason a retry is a retry. It is
 * deliberately NOT in the URL — a key in a query string would be copied into
 * browser history and into any link the customer shares — and deliberately not
 * in storage of any kind: ADR 0012 §2 names exactly one permitted storage key
 * in this app and it is not a payment's.
 */
const pendingTopUp = ref<TopUpAttempt | undefined>(undefined);

/** The offer whose request is in flight, or `null`. Drives the disabled state. */
const committing = ref<string | null>(null);

/** The last refusal from a top-up attempt, as the seam parsed it. */
const commitFailure = ref<Failure | undefined>(undefined);

/**
 * The offer the last commit was for, so the matrix's own retry can re-attempt
 * it instead of doing nothing.
 *
 * `upstream_unavailable` is the row the failure matrix declares `retry` for,
 * and the contract's note on that `503` says the payment is durable and that a
 * retry "under the same idempotency key converges on that same payment and
 * obtains the instructions this attempt could not". So the retry button the matrix
 * puts on screen has to send the SAME request: the same offer, whose key
 * `beginTopUp` reuses because the failed attempt never retired. A retry wired
 * to nothing would be an affordance that promises the customer a second
 * attempt and silently does not take one, which is worse than no button at
 * all; and a retry that took its offer from anywhere but this ref could send a
 * DIFFERENT request under a key that is still live for the first one.
 */
const lastOffered = ref<TopUpOffer | undefined>(undefined);

/** The payment the last successful attempt returned, so its state can be explained. */
const lastAnswer = ref<PaymentIntent | undefined>(undefined);

onMounted(() => {
  void offers.run();
});

/**
 * Start — or retry — a top-up for one offer.
 *
 * The key comes from `beginTopUp`, which reuses the live attempt's key exactly
 * when this is an attempt at the same offer. The guard at the top is the first
 * line of defence against a double fire and the key is the backstop, not the
 * other way round: the buttons below are disabled while a request is in flight,
 * and this refuses a second request that got past them.
 *
 * A FAILED attempt never retires the key. A `503` leaves a durable payment in
 * `created` with no transfer instructions, and the retry the customer makes has
 * to carry the same key for the server to converge on it rather than open a
 * second payment. Only an answer carrying instructions closes the attempt — see
 * `retireTopUp`.
 *
 * A converged answer is RENDERED, never acted on. The contract's note on
 * `idempotency_key` is exact about this: a payment a repeated key converges on
 * "may be in any state by then, including one that has already succeeded", and
 * the honest thing to do with that answer is "render what it was told" — a retry
 * after a customer paid must not present them with a destination to send money
 * to again. `payable` below is the one thing that decides what this handler's
 * answer puts on screen, and it is where that rule lives.
 */
async function startTopUp(offer: TopUpOffer): Promise<void> {
  if (committing.value !== null) return;

  const attempt = beginTopUp(pendingTopUp.value, offer.id);
  pendingTopUp.value = attempt;
  lastOffered.value = offer;
  committing.value = offer.id;
  commitFailure.value = undefined;
  lastAnswer.value = undefined;

  try {
    const result = await createPaymentForOffer({
      offer: offer.id,
      idempotency_key: attempt.key,
    });

    if (!result.ok) {
      commitFailure.value = result.failure;
      return;
    }

    lastAnswer.value = result.data;
    pendingTopUp.value = retireTopUp(pendingTopUp.value, result.data);
    // The new payment is the newest row of the first page. Re-reading keeps the
    // list honest when the page does not change — and the customer's own refresh
    // is the same read happening again.
    payments.run();
  } finally {
    committing.value = null;
  }
}

/**
 * The matrix's `retry` for a refused commit, aimed at the same act.
 *
 * Only ever reached when the failure row says `retry` — `conflict`, `internal`
 * and the non-retryable codes render no button — and it re-enters
 * `startTopUp`, which reuses the live attempt's key for the same offer. That is
 * the retry the contract's `503` describes, and it is deliberately the same
 * function the chooser's buttons call rather than a second path: one commit
 * path is one place for the key rule to hold.
 */
function retryLastTopUp(): void {
  if (lastOffered.value !== undefined) void startTopUp(lastOffered.value);
}

/**
 * What a chooser button says: the price, in the offer's own currency and
 * exponent, and the offer's name.
 *
 * **There is no absent-label branch, and that is a decision rather than an
 * oversight.** It used to read `offer.label === undefined ? \`Add ${price}\` :
 * …`, defending against an offer the schema then made optional. The contract
 * makes `label` REQUIRED now, and it makes it required in the strong sense:
 * the requirement is a deployment's own, an offer declared without a label is
 * refused where it is declared, and a whitespace-only one is refused exactly
 * as a missing one is — so a running server this console can talk to cannot
 * hold an offer that lacks one. A guard keyed on `undefined` for a field the
 * generated type declares `string` is therefore a branch no test can
 * construct and no deployment can reach, and this console does not keep
 * absent-ness guards for fields the contract forbids to be absent: every other
 * `!== undefined` in this package reads a field the contract marks OPTIONAL
 * (`scope?`, `cancel_at?`, `user_count?`, a resource's `data`), which is the
 * distinction that makes them live rather than decorative.
 *
 * What is given up is named honestly: if a server violated the contract —
 * this seam does not runtime-validate a parsed body, so no field on this page
 * is checked at the wire — a labelless offer would render "Add 25.00 EUR —
 * undefined" rather than "Add 25.00 EUR". That is a visibly broken string for
 * a state the contract cannot produce, and it is the price of not carrying an
 * untested second code path through the one screen where a price is shown.
 */
function offerAction(offer: TopUpOffer): string {
  return `Add ${formatPrice(offer)} — ${offer.label}`;
}

const offerList = computed<readonly TopUpOffer[]>(() => offers.data.value?.items ?? []);

/**
 * Whether the offer read has answered, whatever the answer was.
 *
 * The control is ABSENT when the deployment publishes nothing, and the sentence
 * that replaces it is a claim about the deployment — so it must not be made
 * before the deployment has been asked. `data === undefined` is the probe that
 * separates "no offers" from "not answered yet"; the same distinction
 * `resource.ts` is built around, and the reason `loading` alone is not it.
 */
const offersAnswered = computed(() => offers.data.value !== undefined);

/**
 * The one payment whose instructions this page presents, with those
 * instructions, or `undefined` when there is no payment to pay.
 *
 * The contract leaves this to the client and says so — "Whether a destination
 * should be SHOWN is a question about the status beside this field, and it is a
 * client's to answer" — and `showTransferInstructions` is that answer. What is
 * decided HERE is only which payment gets the panel, and there are two cases,
 * both of them the customer looking for where to send money:
 *
 *   - The payment the customer just opened. `lastAnswer` is the direct answer to
 *     the button they pressed, and it is available before the list re-read has
 *     landed, so the instructions appear the moment the server hands them back.
 *   - Otherwise the newest payment on the list that can still be paid into. A
 *     customer who has left and come back has no in-memory answer, and this is
 *     how they find the account again — the capability the old row link carried
 *     for a hosted page, now that there is no URL to link to.
 *
 * One panel rather than one per payable row. The panel is an INSTRUCTION — the
 * full account, bank, holder and drawing — and the table is the HISTORY; a
 * screen that repeated the whole block under every unpaid row would make the
 * list unreadable for the customer who has one to pay and several they once
 * did, and would ask a reader to work out which block was newest. The table's
 * own destination column still names every payable row, so nothing is hidden:
 * only the richest presentation is singular, and it is the newest one.
 */
const payable = computed<
  { readonly payment: PaymentIntent; readonly instructions: TransferInstructions } | undefined
>(() => {
  if (lastAnswer.value !== undefined) {
    const instructions = showTransferInstructions(lastAnswer.value);
    if (instructions !== undefined) return { payment: lastAnswer.value, instructions };
  }
  for (const row of payments.rows.value) {
    const instructions = showTransferInstructions(row);
    if (instructions !== undefined) return { payment: row, instructions };
  }
  return undefined;
});

/**
 * The account number to show in a row's destination cell, or `undefined` when
 * the row has none to act on.
 *
 * A string rather than the whole instruction block because a cell holds one
 * value and the panel is where the bank, the holder and the drawing live. It
 * answers the same question `payable` answers, through the same function, so a
 * row and the panel cannot disagree about whether a payment is payable.
 */
function destinationAccount(row: PaymentIntent): string | undefined {
  return showTransferInstructions(row)?.transfer_code;
}

/**
 * The states present in the list that the provider has not confirmed yet.
 *
 * A customer who has just opened a payment lands here, and this is what the
 * screen says to them: the payment is where the provider left it, and their own
 * return — or their own say-so — did not move it. Set-ordered by the list
 * itself, so the sentence a reader sees is about a payment they can see.
 */
const unconfirmed = computed<readonly PaymentIntentState[]>(() => {
  const seen: PaymentIntentState[] = [];
  for (const row of payments.rows.value) {
    if (!PAYMENT_AWAITING_PROVIDER_STATES.has(row.status)) continue;
    if (!seen.includes(row.status)) seen.push(row.status);
  }
  return seen;
});

const columns: readonly DataTableColumn[] = [
  { key: "created_at", label: "Created" },
  { key: "amount_minor_units", label: "Amount", align: "right" },
  { key: "currency", label: "Currency" },
  { key: "status", label: "Status" },
  { key: "expires_at", label: "We wait until" },
  { key: "transfer_instructions", label: "Destination account" },
];

/**
 * The payments table's state word: three states rather than the two
 * `loading ? "loading" : "empty"` derives, for the reason `CommercePage` and
 * `AccountingPage` both state at their own tables. The read fires from
 * `onMounted`, after the first render, so on the first paint `loading` is false
 * and `data` is `undefined` — and "This account has made no payment." is a
 * claim about the account's money that a read which has not answered cannot
 * support.
 */
function paymentsTableState(): "ready" | "loading" | "empty" {
  if (payments.loading.value) return "loading";
  if (payments.data.value === undefined) return "loading";
  return payments.rows.value.length === 0 ? "empty" : "ready";
}
</script>

<template>
  <Stack gap="lg">
    <PageHeader
      title="Payments"
      description="Fund this account by buying one of the offers this deployment publishes."
    />

    <section aria-labelledby="payments-topup" class="flex flex-col gap-3">
      <h2 id="payments-topup" class="text-lg font-semibold text-foreground">Add funds</h2>

      <FailureView
        v-if="offers.failure.value"
        :failure="offers.failure.value"
        :on-retry="() => offers.run()"
        :retrying="offers.loading.value"
      />

      <LoadingState v-else-if="!offersAnswered" label="Reading this deployment's top-up offers" />

      <p v-else-if="offerList.length === 0" class="text-sm text-muted-foreground">
        This deployment publishes no top-up offer, so there is nothing to buy here. That is a
        perfectly valid deployment — funding for this account is arranged outside this console — and
        it is not an error. Anything already credited to the account still appears on the accounting
        screen.
      </p>

      <template v-else>
        <p class="text-sm text-muted-foreground">
          Choosing an offer opens a payment and shows you an account to transfer into. The money
          moves as a bank transfer, and this console never sees your bank details — no field on this
          page asks for any.
        </p>

        <ul aria-label="Top-up offers" class="flex flex-wrap gap-2">
          <li v-for="offer in offerList" :key="offer.id">
            <Button
              type="button"
              class="min-h-11"
              :disabled="committing !== null"
              :loading="committing === offer.id"
              @click="startTopUp(offer)"
            >
              {{ offerAction(offer) }}
            </Button>
          </li>
        </ul>

        <p v-if="lastAnswer" class="text-sm text-muted-foreground">
          {{ PAYMENT_STATE_EXPLANATION[lastAnswer.status] }}
        </p>

        <FailureView
          v-if="commitFailure"
          :failure="commitFailure"
          :on-retry="retryLastTopUp"
          :retrying="committing !== null"
        />
      </template>
    </section>

    <section v-if="payable" aria-labelledby="payments-transfer" class="flex flex-col gap-3">
      <h2 id="payments-transfer" class="text-lg font-semibold text-foreground">
        Pay by bank transfer
      </h2>
      <TransferInstructionsPanel :payment="payable.payment" :instructions="payable.instructions" />
    </section>

    <section aria-labelledby="payments-list" class="flex flex-col gap-3">
      <h2 id="payments-list" class="text-lg font-semibold text-foreground">Payments</h2>

      <Card v-if="unconfirmed.length > 0">
        <p class="text-sm font-medium text-foreground">
          We are waiting for your provider to confirm.
        </p>
        <p class="mt-1 text-sm text-muted-foreground">
          A payment's status is written only by a signature-verified message from your provider's
          own servers. This page cannot see your transfer until your provider reports it, so a
          payment that still reads as waiting here is one the provider has not spoken about yet.
        </p>
        <ul class="mt-2 flex list-disc flex-col gap-1 pl-5 text-sm text-muted-foreground">
          <li v-for="state in unconfirmed" :key="state">
            {{ PAYMENT_STATE_PRESENTATION[state].label }}: {{ PAYMENT_STATE_EXPLANATION[state] }}
          </li>
        </ul>
      </Card>

      <FailureView
        v-if="payments.failure.value"
        :failure="payments.failure.value"
        :on-retry="() => payments.run()"
        :retrying="payments.loading.value"
      />

      <DataTable
        v-else
        caption="Payments made by this account, newest first"
        layer="page"
        :columns="columns"
        :rows="payments.rows.value"
        :state="paymentsTableState()"
        :pages="payments.pages.value"
        empty-message="This account has made no payment."
        note="A payment's amount is the price its offer published. It is not a balance, and the money it credits is on the accounting screen."
      >
        <template #created_at="{ row }: { row: PaymentIntent }">
          <InstantCell :value="row.created_at" />
        </template>
        <template #amount_minor_units="{ row }: { row: PaymentIntent }">
          <span class="font-mono text-sm tabular-nums">{{ formatPriceAmount(row) }}</span>
        </template>
        <template #currency="{ row }: { row: PaymentIntent }">
          <span class="font-mono text-xs">{{ row.currency }}</span>
        </template>
        <template #status="{ row }: { row: PaymentIntent }">
          <StatusBadge :status="PAYMENT_STATE_PRESENTATION[row.status]" />
        </template>
        <template #expires_at="{ row }: { row: PaymentIntent }">
          <InstantCell :value="row.expires_at" />
        </template>
        <template #transfer_instructions="{ row }: { row: PaymentIntent }">
          <!-- The account a customer can pay into, selectable so it can be
               copied rather than retyped. It is shown exactly while the payment
               is one the provider has not settled, and the three branches are
               three different facts: a destination to pay, a payment whose
               destination was never obtained, and a payment that is over and
               whose account must not be paid again. -->
          <span v-if="destinationAccount(row) !== undefined" class="select-all font-mono text-xs">
            {{ destinationAccount(row) }}
          </span>
          <span
            v-else-if="row.transfer_instructions === null"
            class="text-xs text-muted-foreground"
          >
            Not ready yet
          </span>
          <span v-else class="text-xs text-muted-foreground">—</span>
        </template>
      </DataTable>

      <p class="text-xs text-muted-foreground">
        Each row is one payment, rendered from the server's own fields. The destination account is
        the provider's own account number, handed to the browser as it arrived: this console does
        not derive it, does not build a payment code of its own, and nothing here decides that a
        payment succeeded. The account is shown only while the provider still has something to
        confirm; on a settled payment it reads as a dash, because transferring to it again would
        send money to an account whose payment is already over.
      </p>
    </section>

    <Card>
      <p class="text-sm text-muted-foreground">
        A payment's status is written only by a signature-verified webhook from the provider's own
        servers. The money moves as a bank transfer to an account the provider holds for the
        payment; this console never sees your bank details, and refreshing this page is a status
        re-read and nothing more.
      </p>
    </Card>
  </Stack>
</template>
