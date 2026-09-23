<script setup lang="ts">
// Public Loom imports only. Icon defaults must run in a component setup so
// Loom can provide them to the component tree (rather than silently doing
// nothing from the entry module).
import { applyLoomIconDefaults, SkipLink } from "@ecoma-io/loom";
import { RouterView } from "vue-router";

import AppLayout from "@/layouts/AppLayout.vue";

applyLoomIconDefaults();
</script>

<template>
  <!-- First focusable element: Loom's skip link targets the `main` landmark
       rendered by AppLayout, preserving a keyboard route past shell chrome. -->
  <SkipLink />
  <AppLayout>
    <RouterView />
  </AppLayout>
</template>
