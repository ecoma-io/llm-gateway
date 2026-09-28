<script setup lang="ts">
// The one component a money figure is rendered through. A screen does not
// hand-roll a `<span>` of grouped digits: it hands the `Money` to this, and
// this decides the sign presentation and the prose that names the unit.
//
// The unit is named in the accessible text, not painted as a symbol, because
// the `Money` schema carries no currency: there is no symbol to paint. What a
// screen-reader user hears is "1,250 minor units", which is the truth the
// contract states; what a sighted user sees is the same digits, with the unit
// in the surrounding column header. Colour is not used — a money figure is
// not a status, and a green "balance" would be a claim about its health the
// contract never makes.
//
// The sign presentation is the caller's: `signed` is for a `delta`, whose
// sign is meaningful, and off for a `balance`, which this plane refuses to
// store negative — so a `-` on a balance is an anomaly the reader should see
// in its raw grouped form, not dressed up as a direction.
import { computed } from "vue";

import { formatMoney, formatSignedMinorUnits, UNRENDERABLE_AMOUNT } from "./money";
import type { Money } from "@ecoma-io/llm-gateway-console-api-client";

const props = withDefaults(
  defineProps<{
    /** The amount. `undefined`/`null` renders the unrenderable sentinel. */
    amount: Money | undefined | null;
    /**
     * Render this as a signed delta (a ledger leg's movement) rather than a
     * balance. Off by default.
     */
    signed?: boolean;
  }>(),
  { signed: false },
);

const rendered = computed(() =>
  props.signed ? formatSignedMinorUnits(props.amount) : formatMoney(props.amount),
);

/**
 * The unrenderable case is a data problem, so it is announced as one rather
 * than read out as a number a reader would take at face value. The
 * `aria-label` overrides the digits only in that case; a normal amount has
 * no label override and the surrounding header names the unit once.
 */
const isRenderable = computed(() => rendered.value !== UNRENDERABLE_AMOUNT);
</script>

<template>
  <span
    class="font-mono tabular-nums"
    :aria-label="isRenderable ? undefined : 'Amount too large to display exactly'"
    :title="
      isRenderable ? undefined : 'This amount is outside the range that can be shown exactly.'
    "
    >{{ rendered }}</span
  >
</template>
