<script setup lang="ts">
// The shell. It is chrome, not a product screen: the sidebar, the header, the
// skip-link target and the sign-out control, and nothing that reads a product
// operation.
//
// **The nav lists only what exists.** Every entry here is a route with a screen
// behind it, and the screens that ADR 0012 §5 rules out are absent rather than
// disabled: a nav link to a screen with no contract behind it is a promise the
// console cannot keep, and a disabled link is a dead end an operator stops
// reading past.
//
// **The sign-out is a real control and it says what it does.** It ends the
// session server-side, and the local state is cleared whether or not the call
// succeeded — leaving a console wearing a signed-in shell after a failed
// sign-out is the failure ADR 0012 §2 names.
import {
  AppHeader,
  AppShell,
  Button,
  SidebarNav,
  type SidebarNavSection,
  useTheme,
} from "@ecoma-io/loom";
import {
  BookOpen,
  CircleDollarSign,
  LayoutDashboard,
  Moon,
  Monitor,
  Receipt,
  Sun,
  Users,
  Wallet,
} from "@lucide/vue";
import { computed } from "vue";
import { RouterLink, useRoute, useRouter } from "vue-router";

import { useSessionStore } from "@/stores/session";

const route = useRoute();
const router = useRouter();
const session = useSessionStore();
const { theme, setTheme } = useTheme();

/**
 * The console's screens, in the order an operator moves through them: what this
 * account is, who is in it, what they may buy, how they fund it, what they have
 * been granted, where the money is, and whether the numbers agree.
 *
 * `active` is computed from the route path rather than declared, so a link is
 * current whenever the reader is on its screen — including on a query change
 * inside that screen, which is where paged reads and filters live.
 */
const navigation = computed<SidebarNavSection[]>(() => [
  {
    items: [
      { icon: LayoutDashboard, label: "Dashboard", href: "/", active: route.path === "/" },
      { icon: Users, label: "Identity", href: "/identity", active: route.path === "/identity" },
      { icon: BookOpen, label: "Catalog", href: "/catalog", active: route.path === "/catalog" },
      { icon: Receipt, label: "Commerce", href: "/commerce", active: route.path === "/commerce" },
      // Between buying and accounting, which is the order the money moves in: an
      // offer is what a customer CHOOSES to buy, a payment is the attempt at
      // buying it, and the ledger behind `/accounting` is what the provider's
      // confirmation eventually writes. A reader looking for "how do I add
      // funds" scans for a word about funding, and "Payments" is the word this
      // contract uses for the attempt.
      //
      // `Wallet` and NOT a card glyph: this console has no card field anywhere,
      // and an icon of one on the nav would be chrome promising a form that
      // does not exist and must not.
      {
        icon: Wallet,
        label: "Payments",
        href: "/payments",
        active: route.path === "/payments",
      },
      {
        icon: CircleDollarSign,
        label: "Accounting",
        href: "/accounting",
        active: route.path === "/accounting",
      },
      {
        icon: Receipt,
        label: "Reconciliation",
        href: "/reconciliation",
        active: route.path === "/reconciliation",
      },
    ],
  },
]);

const themeOptions = [
  { label: "Light", value: "light" as const, icon: Sun },
  { label: "Dark", value: "dark" as const, icon: Moon },
  { label: "System", value: "system" as const, icon: Monitor },
];

/** The address of the signed-in identity, when the session carried one. It is a label, never a lookup key. */
const signedInAs = computed(() => session.principal?.email);

async function signOut() {
  await session.end();
  await router.replace("/sign-in");
}
</script>

<template>
  <AppShell sidebar-aria-label="Console navigation">
    <template #sidebar>
      <SidebarNav :sections="navigation" aria-label="Console navigation" />
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

        <!--
          `AppHeader`'s right-hand region. Loom names the slot `notifications`,
          which is a misnomer for what a console puts there — it is the trailing
          slot, and a session's identity and sign-out are exactly what belongs
          in it. Filling the named slot is preferable to reaching past the
          primitive for an unnamed one.
        -->
        <template #notifications>
          <p v-if="signedInAs" class="text-sm text-muted-foreground">
            Signed in as {{ signedInAs }}
          </p>
          <Button variant="secondary" type="button" class="min-h-11" @click="signOut">
            Sign out
          </Button>
        </template>
      </AppHeader>
    </template>

    <!--
      `tabindex="-1"` is what makes the skip link's target real: a `main` that
      is not focusable cannot receive focus, and a skip link that moves the
      viewport without moving focus leaves a keyboard user where they were.
    -->
    <main id="main" tabindex="-1" class="py-8">
      <slot />
    </main>
  </AppShell>
</template>
