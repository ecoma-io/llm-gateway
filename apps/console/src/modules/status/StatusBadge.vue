<script setup lang="ts">
// The ONE place a status is rendered. Every screen routes its enums here
// rather than reaching for a `Badge` and picking a colour, because a badge
// that is handed a `variant` is a badge three channels away from being
// colour-only, and the defect this component exists to prevent is a single
// `<Badge :variant>` away.
//
// The icon is `aria-hidden` on purpose: the label beside it is the state's
// name, and an `aria-hidden="false"` icon would add a second spoken name for
// the same fact. The shape is for a sighted reader in greyscale; the word is
// for everyone else.
import { Badge } from "@ecoma-io/loom";

import type { StatusPresentation } from "./presentation";

defineProps<{
  status: StatusPresentation;
}>();
</script>

<template>
  <Badge :variant="status.tone" class="inline-flex items-center gap-1.5">
    <component :is="status.icon" aria-hidden="true" class="size-3.5 shrink-0" />
    <span>{{ status.label }}</span>
  </Badge>
</template>
