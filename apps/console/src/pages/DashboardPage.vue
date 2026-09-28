<script setup lang="ts">
// The dashboard: the account overview the server composed (ADR 0012 §1).
//
// **Nothing here is arithmetic.** The figures on this screen arrive as a
// composed projection from three bounded contexts, and each is rendered from
// its own field. A page that summed the PAYG buckets' `available` figures, or
// added `held` to `settled`, or counted the subscriptions it was shown, would
// be doing the plane's work in the browser where a disagreement has no
// authority behind it — and two of those would be actively wrong: the
// subscription list is bounded by the contract's own `maximum` and is not the
// account's whole set, and the bucket list is paginated and is not the account's
// whole set either.
//
// **The PAYG buckets are shown per bucket, never as one number.** A sum across
// buckets is exactly the "plan total" ADR 0012 §4 refuses: each bucket is a
// separate balance with a separate owner (a cycle that rolls must not take the
// pay-as-you-go balance with it), and one figure over several of them is a
// number that answers no question the rows do not answer better.
//
// **Every figure the contract marks optional is rendered as absent when it is
// absent.** `user_count` and `active_api_key_count` are optional because a
// composition may decline to compute them; a screen that showed `0` for an
// absent count would be asserting that the account has no users.
import { Button, Card, PageHeader, Stack } from "@ecoma-io/loom";
import { computed, onMounted } from "vue";
import { RouterLink } from "vue-router";

import { fetchAccountOverview } from "@/lib/api";
import { useResource } from "@/lib/resource";
import FailureView from "@/modules/failure/FailureView.vue";
import MoneyAmount from "@/modules/money/MoneyAmount.vue";
import StatusBadge from "@/modules/status/StatusBadge.vue";
import {
  ACCOUNT_STATE_PRESENTATION,
  SUBSCRIPTION_STATE_PRESENTATION,
} from "@/modules/status/presentation";
import type {
  AccountOverview,
  FundingBucket,
  Subscription,
} from "@ecoma-io/llm-gateway-console-api-client";

const overview = useResource<AccountOverview>(() => fetchAccountOverview());

/**
 * The subscriptions the composition carried, as rows the screen renders.
 *
 * The response says this list is bounded by the contract's own `maximum`, and a
 * dashboard showing three of eleven is honest where one claiming to show all
 * eleven is making a promise this plane will not keep. The bound is RESTATED on
 * screen for the same reason: a truncated list that does not say it is
 * truncated reads as a complete one.
 */
const subscriptions = computed<readonly Subscription[]>(
  () => overview.data.value?.subscriptions ?? [],
);

const paygBuckets = computed<readonly FundingBucket[]>(
  () => overview.data.value?.payg_balances ?? [],
);

/** A count the contract made optional, rendered as absent rather than as zero. */
function count(value: number | undefined): string {
  return value === undefined ? "Not reported" : value.toLocaleString("en-US");
}

onMounted(() => {
  void overview.run();
});
</script>

<template>
  <Stack gap="lg">
    <PageHeader
      title="Dashboard"
      description="This account, composed by the server from the figures it holds."
    >
      <template #actions>
        <Button :loading="overview.loading.value" class="min-h-11" @click="() => overview.run()">
          Refresh
        </Button>
      </template>
    </PageHeader>

    <FailureView
      v-if="overview.failure.value"
      :failure="overview.failure.value"
      :on-retry="() => overview.run()"
      :retrying="overview.loading.value"
    />

    <template v-if="overview.data.value">
      <Card>
        <div class="flex flex-wrap items-start justify-between gap-4">
          <div>
            <p class="text-sm text-muted-foreground">Account</p>
            <p class="text-xl font-semibold text-foreground">
              {{ overview.data.value.account.name }}
            </p>
            <p class="mt-1 font-mono text-xs text-muted-foreground">
              {{ overview.data.value.account.id }}
            </p>
          </div>
          <StatusBadge :status="ACCOUNT_STATE_PRESENTATION[overview.data.value.account.state]" />
        </div>
      </Card>

      <!--
        Two counts and one whole-plane number. `open_finding_count` is a
        fact about the system rather than about this account — the contract says
        so on the field — so its card says so too, and a reader does not learn
        from it that this customer owes anything.
      -->
      <section aria-label="Account figures" class="grid gap-4 sm:grid-cols-3">
        <Card title="Live users" :description="count(overview.data.value.user_count)">
          <p class="text-2xl font-semibold text-foreground">
            {{
              overview.data.value.user_count === undefined ? "—" : overview.data.value.user_count
            }}
          </p>
        </Card>
        <Card
          title="Active API keys"
          :description="count(overview.data.value.active_api_key_count)"
        >
          <p class="text-2xl font-semibold text-foreground">
            {{
              overview.data.value.active_api_key_count === undefined
                ? "—"
                : overview.data.value.active_api_key_count
            }}
          </p>
        </Card>
        <Card
          title="Open reconciliation findings"
          description="Across the whole system, not this account alone."
        >
          <p class="text-2xl font-semibold text-foreground">
            {{
              overview.data.value.open_finding_count === undefined
                ? "—"
                : overview.data.value.open_finding_count
            }}
          </p>
        </Card>
      </section>

      <!--
        The pay-as-you-go balances, one card per bucket. Each carries its own
        three figures and NO total across them: a bucket is a cycle's or an
        account's own balance, and adding two of them together is a number the
        ledger has never asserted.
      -->
      <section aria-label="Pay-as-you-go balances" class="flex flex-col gap-3">
        <h2 class="text-lg font-semibold text-foreground">Pay-as-you-go balances</h2>
        <p v-if="paygBuckets.length === 0" class="text-sm text-muted-foreground">
          This account has no pay-as-you-go bucket. A pay-as-you-go balance is separate from any
          subscription, and neither stands in for the other.
        </p>
        <ul v-else class="grid gap-4 sm:grid-cols-2">
          <li v-for="bucket in paygBuckets" :key="bucket.id">
            <Card :title="`Bucket ${bucket.id}`" :description="`Kind: ${bucket.kind}`">
              <dl class="grid grid-cols-3 gap-3">
                <div>
                  <dt class="text-xs text-muted-foreground">Settled</dt>
                  <dd><MoneyAmount :amount="bucket.balances.settled" /></dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">Held</dt>
                  <dd><MoneyAmount :amount="bucket.balances.held" /></dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">Available</dt>
                  <dd><MoneyAmount :amount="bucket.balances.available" /></dd>
                </div>
              </dl>
              <p class="mt-3 text-xs text-muted-foreground">
                Minor units. Rendered, never derived.
              </p>
            </Card>
          </li>
        </ul>
        <RouterLink
          to="/accounting"
          class="inline-flex min-h-11 w-fit items-center rounded-md border border-border px-4 text-sm underline-offset-4 hover:underline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring"
        >
          Open accounting
        </RouterLink>
      </section>

      <!--
        The subscription rows. A subscription is a recurring arrangement and a
        pay-as-you-go bucket is not one, so this list is a list of
        subscriptions and nothing here calls it a total — see the card below.
      -->
      <section aria-label="Subscriptions" class="flex flex-col gap-3">
        <h2 class="text-lg font-semibold text-foreground">Subscriptions</h2>
        <p v-if="subscriptions.length === 0" class="text-sm text-muted-foreground">
          This account has no subscription. A subscription and a pay-as-you-go balance are different
          arrangements, and this account has neither beyond what is listed here.
        </p>
        <ul v-else class="flex flex-col gap-2">
          <li
            v-for="subscription in subscriptions"
            :key="subscription.id"
            class="flex flex-wrap items-center justify-between gap-3 rounded-md border border-border px-4 py-3"
          >
            <div class="flex flex-col">
              <span class="font-mono text-sm text-foreground">{{ subscription.id }}</span>
              <span class="text-xs text-muted-foreground">
                Plan version {{ subscription.plan_version_id }}
              </span>
            </div>
            <div class="flex flex-wrap items-center gap-3">
              <span v-if="subscription.cancel_at" class="text-xs text-muted-foreground">
                Ends {{ subscription.cancel_at }}
              </span>
              <StatusBadge :status="SUBSCRIPTION_STATE_PRESENTATION[subscription.state]" />
            </div>
          </li>
        </ul>
        <p class="text-xs text-muted-foreground">
          The most recent subscriptions are shown here, not all of them. The full list, with each
          arrangement's state, is on the commerce screen.
        </p>
      </section>
    </template>

    <p
      v-else-if="!overview.loading.value && !overview.failure.value"
      class="text-sm text-muted-foreground"
    >
      Nothing has loaded yet.
    </p>
  </Stack>
</template>
