<script setup lang="ts">
// The destination a customer pays into, rendered IN PLACE and never navigated
// to.
//
// There is no hosted page to send the browser to: the instrument is a domestic
// bank transfer to a virtual account, so this console's job at this point is to
// put the account, the bank, the holder and the provider's drawing on screen and
// stop. Nothing here fetches `qr_url`, resolves it, or reads an amount or an
// account out of it — the image is the provider's own, handed to an `<img>`
// exactly as it arrived, because the contract says every member of the
// instructions is returned verbatim and never parsed, and a client that
// interpreted one would break the day the provider changed its encoding.
//
// Every value is the payment's own and none is derived here: the amount is
// `amount_minor_units` with the point placed from `minor_unit_exponent` by the
// shared formatter, the account is `transfer_code`, and the deadline is
// `expires_at`, which the contract calls a deadline on this platform's PATIENCE
// rather than on the money — a delivery arriving after it is still honoured, so
// the column is titled for the waiting and not as a moment after which the
// customer has lost their money.
//
// The last paragraph is what the whole screen exists to say: the payment is
// successful only when the provider's own servers confirm the transfer, this
// page has no part in that, and the customer may leave. A redirect used to say
// the last part silently — the browser left, so of course the customer could —
// and a page that renders in place has to say it in words instead.
import { Card } from "@ecoma-io/loom";

import { formatPrice } from "@/modules/payments/offer-price";
import type { TransferInstructions } from "@/modules/payments/instructions";
import InstantCell from "@/modules/status/InstantCell.vue";
import type { PaymentIntent } from "@ecoma-io/llm-gateway-console-api-client";

defineProps<{
  /** The payment these instructions pay. */
  readonly payment: PaymentIntent;
  /** Where to send the money. Never null here: the caller decides what to show. */
  readonly instructions: TransferInstructions;
}>();
</script>

<template>
  <Card>
    <dl class="grid gap-x-8 gap-y-2 text-sm sm:grid-cols-2">
      <div>
        <dt class="text-muted-foreground">Amount due</dt>
        <dd class="font-mono tabular-nums text-foreground">{{ formatPrice(payment) }}</dd>
      </div>
      <div>
        <dt class="text-muted-foreground">Account number</dt>
        <!-- Selectable on purpose: the value a customer pays into is one they
             may need to copy into a banking app, and a number they cannot
             select is one they retype and mistype. -->
        <dd class="select-all font-mono text-foreground">{{ instructions.transfer_code }}</dd>
      </div>
      <div>
        <dt class="text-muted-foreground">Bank</dt>
        <dd class="text-foreground">{{ instructions.bank_name }}</dd>
      </div>
      <div>
        <dt class="text-muted-foreground">Account holder</dt>
        <dd class="text-foreground">{{ instructions.account_holder }}</dd>
      </div>
      <div>
        <dt class="text-muted-foreground">We wait until</dt>
        <dd class="text-foreground"><InstantCell :value="payment.expires_at" /></dd>
      </div>
    </dl>

    <!-- The QR is the provider's own drawing, shown when the provider made one.
         A null `qr_url` is a fact about the provider's answer and not a failure:
         the account number above is always enough to pay, and the sentence in
         the null branch says so rather than leaving the customer to guess. -->
    <img
      v-if="instructions.qr_url"
      :src="instructions.qr_url"
      alt="QR code your provider drew for this transfer"
      class="mt-4 size-40 border border-border"
    />
    <p v-else class="mt-3 text-sm text-muted-foreground">
      Your provider drew no QR code for this transfer. Type the account number above into your
      banking app — it is all you need, and nothing about this payment is wrong.
    </p>

    <p class="mt-4 text-sm text-muted-foreground">
      Send the amount above to that account. This payment becomes successful only when your
      provider's own servers confirm the transfer: this page cannot mark it paid, and nothing here
      does. You can leave this page — the transfer does not need it open, and refreshing is how you
      will learn the outcome.
    </p>
  </Card>
</template>
