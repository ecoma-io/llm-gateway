<script setup lang="ts">
// Identity: the people in this account and the keys that act for it.
//
// Two lists, one screen, and nothing here invents a relationship between them.
// A user is a human with a lifecycle; a key is a credential with a prefix. The
// key's `created_by` names the console identity that minted it, which is often
// nobody — a key provisioned by CI carries no user — and the list is not
// sorted to make that look like a link it is not. Both lists page on their own
// cursors in the same route query, so moving one and then the other is two
// positions the console cannot name and must re-ask for.
//
// **The key name and the key's own credential are different things, and only
// one of them is ever on this page.** `display_name` and `prefix` are the two
// identifying facts the contract returns for a key, and a key's plaintext
// exists in exactly one response, rendered once by `OneTimeSecret` and erased
// on the way out. There is no column here that could hold a secret, because
// there is no operation that returns one.
//
// The mint lives on this screen because it is the only write in the first
// console, and it is a `user`-class write: an `operator` may read the
// account's keys and may not mint one. The contract is the authority for that
// and this page does not re-derive it, so the control is rendered for every
// session the guard admits and a `403`-shaped refusal is answered by the
// matrix.
import { Card, PageHeader, SegmentedControl, Stack } from "@ecoma-io/loom";
import { computed } from "vue";

import DataTable from "@/components/DataTable.vue";
import { fetchApiKeys, fetchUsers } from "@/lib/api";
import { usePagedList } from "@/lib/paged-list";
import type { DataTableColumn } from "@/components/data-table";
import FailureView from "@/modules/failure/FailureView.vue";
import InstantCell from "@/modules/status/InstantCell.vue";
import OneTimeSecret from "@/modules/session/OneTimeSecret.vue";
import StatusBadge from "@/modules/status/StatusBadge.vue";
import { API_KEY_STATE_PRESENTATION, USER_STATE_PRESENTATION } from "@/modules/status/presentation";
import type {
  ApiKey,
  ApiKeyPage,
  ListApiKeysData,
  ListUsersData,
  User,
  UserPage,
} from "@ecoma-io/llm-gateway-console-api-client";

/**
 * The lifecycle states, read from the contract's own union rather than from a
 * second list kept beside it. A state added to `console.yaml` lands in the
 * client's types and this array stops type-checking until someone writes the
 * filter for it — the same gate `presentation.ts` uses for its labels.
 */
const USER_STATES: readonly User["state"][] = ["invited", "active", "removed"];

const users = usePagedList<UserPage, ListUsersData["query"]>({
  read: (query) => fetchUsers({ query }),
  // This is the FIRST list on the route, so it keeps the contract's own name
  // for the cursor: `after` is the parameter the contract documents and the one
  // a link this screen hands out should carry, and a reader who arrives with a
  // bare `after` gets what they asked for. That is why the keys list below is
  // the one that moves — naming both would mean a shared link read
  // `users_after=c-users-3`, and a reader who had followed the contract's own
  // documentation would watch their cursor be silently ignored.
  shape: { filters: ["state"], cursor: "after" },
  vocabulary: { state: USER_STATES },
});

const keys = usePagedList<ApiKeyPage, ListApiKeysData["query"]>({
  read: (query) => fetchApiKeys({ query }),
  // The keys list filters on nothing, so it declares no filter keys — and the
  // pager therefore carries no filter into its links. A screen that declared a
  // filter it does not send would put a parameter the operation does not accept
  // into a shared URL, which is the 400 this plane refuses.
  //
  // Its cursor key is NAMED, and that is the whole point of the line. Two lists
  // on one route share one query string, and `cursor` defaults to `after` for
  // every list that does not name its own — so with both left at the default
  // they are writing to the same key. Paging the keys then overwrites the users
  // position with a keys cursor, the users list re-reads, and it sends that
  // cursor against `GET /users`. The server checks that the cursor names the
  // collection it was minted for (`application/consolecursor.go`) and answers
  // 400 "the after cursor names a different collection"; `resource.ts` classes
  // `invalid_request` as a LOST cursor, and `paged-list.ts` responds by firing
  // `restartWithoutCursor(carryOver())` — a REPLACE that drops `after` altogether
  // and takes the keys position with it. One list's Next destroys the other
  // list's position, and the reader is left on page one of both.
  //
  // `AccountingPage` already names its second list `ledger_after`; this is that
  // decision applied to the other two two-list routes. A screen with only one
  // list should leave the default alone — there is nothing to collide with.
  shape: { filters: [], cursor: "keys_after" },
  vocabulary: {},
});

const userColumns: readonly DataTableColumn[] = [
  { key: "email", label: "Email" },
  { key: "state", label: "Lifecycle" },
  { key: "created_at", label: "Created" },
];

const keyColumns: readonly DataTableColumn[] = [
  { key: "display_name", label: "Name" },
  { key: "prefix", label: "Prefix", class: "font-mono text-xs" },
  { key: "state", label: "State" },
  { key: "created_at", label: "Created" },
];

const stateOptions = [
  { value: "", label: "All" },
  ...USER_STATES.map((state) => ({ value: state, label: USER_STATE_PRESENTATION[state].label })),
];

const stateValue = computed({
  get: () => users.filter("state") ?? "",
  set: (value: string) => users.setFilter("state", value),
});

/** The table's state word, from the resource's own four. */
function tableState(): "ready" | "loading" | "empty" {
  if (users.loading.value) return "loading";
  if (users.data.value === undefined) return "loading";
  return users.rows.value.length === 0 ? "empty" : "ready";
}

/**
 * The keys table's state word, and the same three-way answer `tableState` gives
 * for the same reason.
 *
 * `loading.value ? "loading" : "empty"` is a two-state reading of a four-state
 * resource, and it is wrong in the one direction a reader can be harmed by.
 * `usePagedList` fires its read from `onMounted`, which runs AFTER the first
 * render, so on the very first paint the read has not started: `loading` is
 * false, `data` is still `undefined`, and `rows` is the empty array that
 * `resource.ts` initialises. The table therefore renders its empty message —
 * "This account has no API key." — as a confident factual claim about an
 * account the console has not heard from. A reader arriving on a slow
 * connection is told the account has no keys; a reader whose key really is
 * revoked sees the same sentence and cannot tell the two apart.
 *
 * `data === undefined` is the only honest probe of "has anything been answered",
 * because `rows` is empty in both the not-yet-answered case and the genuinely
 * empty case, and those are the two claims a screen must not confuse.
 */
function keysTableState(): "ready" | "loading" | "empty" {
  if (keys.loading.value) return "loading";
  return keys.rows.value.length === 0 ? "empty" : "ready";
}
</script>

<template>
  <Stack gap="lg">
    <PageHeader
      title="Identity"
      description="The people in this account, and the API keys that act for it."
    />

    <section aria-labelledby="identity-users" class="flex flex-col gap-3">
      <h2 id="identity-users" class="text-lg font-semibold text-foreground">Users</h2>

      <!--
        The one filter on this screen, and it is a `SegmentedControl` rather
        than a `<select>` because the vocabulary is three values and all three
        are usually in view. The control is labelled by its own group, so the
        reader is told what the three options filter before reading the three
        options.
      -->
      <SegmentedControl
        v-model="stateValue"
        :options="stateOptions"
        aria-label="Filter users by lifecycle"
      />

      <FailureView
        v-if="users.failure.value"
        :failure="users.failure.value"
        :on-retry="() => users.run()"
        :retrying="users.loading.value"
      />

      <DataTable
        v-else
        caption="Users in this account"
        layer="page"
        :columns="userColumns"
        :rows="users.rows.value"
        :state="tableState()"
        :pages="users.pages.value"
        empty-message="No user in this account matches that lifecycle."
      >
        <template #state="{ row }: { row: User }">
          <StatusBadge :status="USER_STATE_PRESENTATION[row.state]" />
        </template>
        <template #created_at="{ row }: { row: User }">
          <InstantCell :value="row.created_at" />
        </template>
      </DataTable>
    </section>

    <section aria-labelledby="identity-keys" class="flex flex-col gap-3">
      <h2 id="identity-keys" class="text-lg font-semibold text-foreground">API keys</h2>

      <FailureView
        v-if="keys.failure.value"
        :failure="keys.failure.value"
        :on-retry="() => keys.run()"
        :retrying="keys.loading.value"
      />

      <DataTable
        v-else
        caption="API keys for this account"
        layer="page"
        :columns="keyColumns"
        :rows="keys.rows.value"
        :state="keysTableState()"
        :pages="keys.pages.value"
        empty-message="This account has no API key."
      >
        <template #state="{ row }: { row: ApiKey }">
          <StatusBadge :status="API_KEY_STATE_PRESENTATION[row.state]" />
        </template>
        <template #created_at="{ row }: { row: ApiKey }">
          <InstantCell :value="row.created_at" />
        </template>
      </DataTable>
    </section>

    <!--
      The mint, below the lists rather than above them: an operator who came to
      read who is in the account should find that first, and the write is the
      thing they came for when they came for a write.
    -->
    <Card title="Create a key" description="A key is shown once and never again.">
      <OneTimeSecret />
    </Card>
  </Stack>
</template>
