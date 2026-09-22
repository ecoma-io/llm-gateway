<script setup lang="ts">
import {
  AppHeader,
  AppShell,
  Button,
  SidebarNav,
  type SidebarNavSection,
  useTheme,
} from "@ecoma-io/loom";
import { Activity, Moon, Monitor, Sun } from "@lucide/vue";
import { computed } from "vue";
import { RouterLink, useRoute } from "vue-router";

const route = useRoute();
const { theme, setTheme } = useTheme();

// This is shell navigation, not a product module. The single destination is
// intentional: its page is the only API surface the scaffold actually owns.
const navigation = computed<SidebarNavSection[]>(() => [
  {
    items: [
      {
        icon: Activity,
        label: "Gateway status",
        href: "/",
        active: route.path === "/",
      },
    ],
  },
]);

const themeOptions = [
  { label: "Light", value: "light" as const, icon: Sun },
  { label: "Dark", value: "dark" as const, icon: Moon },
  { label: "System", value: "system" as const, icon: Monitor },
];
</script>

<template>
  <AppShell sidebar-aria-label="Console navigation">
    <template #sidebar>
      <SidebarNav :sections="navigation" aria-label="Gateway navigation" />
    </template>

    <template #header>
      <AppHeader aria-label="Console header">
        <template #brand>
          <RouterLink to="/" class="text-body font-semibold text-foreground no-underline">
            Ecoma LLM Gateway
          </RouterLink>
        </template>

        <template #leading>
          <div aria-label="Theme preference" class="flex items-center gap-1" role="group">
            <Button
              v-for="option in themeOptions"
              :key="option.value"
              :aria-pressed="theme === option.value"
              :aria-label="`${option.label} theme`"
              :variant="theme === option.value ? 'secondary' : 'ghost'"
              size="icon-sm"
              type="button"
              @click="setTheme(option.value)"
            >
              <component :is="option.icon" aria-hidden="true" />
              <span class="sr-only">{{ option.label }}</span>
            </Button>
          </div>
        </template>
      </AppHeader>
    </template>

    <main id="main" class="py-8" tabindex="-1">
      <slot />
    </main>
  </AppShell>
</template>
