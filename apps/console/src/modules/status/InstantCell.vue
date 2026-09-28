<script setup lang="ts">
// An instant, in a table cell.
//
// The element is a `<time datetime="…">` whose text is the contract's own
// string and whose `datetime` is that same string — the server's timestamp, not
// a re-serialisation of one this page parsed. The alternative is a console that
// has a second formatting authority, whose answer changes with the reader's
// locale and time zone rather than with the data, and which renders a value the
// contract never sent the moment the two disagree.
//
// The three optional states are the module's and are documented in `instant.ts`:
// a value, `null` (a fact about the row), and `undefined` (not sent, so nothing
// at all is rendered).
import { computed } from "vue";

import { instantDateTime, instantText } from "./instant";
import type { Instant } from "./instant";

const props = defineProps<{ value: Instant }>();

const text = computed(() => instantText(props.value));
const dateTime = computed(() => instantDateTime(props.value));
</script>

<template>
  <time v-if="dateTime" :datetime="dateTime" class="whitespace-nowrap font-mono text-xs">{{
    text
  }}</time>
  <span v-else-if="text !== undefined" class="text-xs text-muted-foreground">{{ text }}</span>
</template>
