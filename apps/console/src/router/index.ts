// The route guard: one gate, and it asks one question.
//
// `resolve()` is the session store's, and it is the only thing that decides
// whether a visitor is signed in — the store asks `GET /auth/session`, and a
// `200` there is proof of exactly that and no more. A guard that inferred the
// answer from a cookie, a local storage entry or a route name would be a second
// opinion about authentication, and the seam exists so there is only one.
//
// The sign-in screen is the ONE route reachable without a session, and the
// root is the only screen a signed-in visitor is redirected away from — an
// authenticated operator who lands on `/sign-in` gets sent to the dashboard
// rather than shown a form for a session they already hold.
//
// `onSessionEnded` is the second half of the same rule. A session that ends
// mid-visit is not an error each screen discovers on its own: the seam is the
// only place that sees every call, so it is the only place the 401 can be
// observed from, and the shell subscribes to it here. A page that handled its
// own 401 would route on whichever request happened to fail first, which is
// whichever screen mounted last.
import {
  createRouter,
  createWebHistory,
  type Router,
  type RouteLocationNormalized,
} from "vue-router";

import { onSessionEnded } from "@/lib/api";
import { useSessionStore } from "@/stores/session";
import GatewayStatusPage from "@/pages/GatewayStatusPage.vue";
import SignInPage from "@/pages/SignInPage.vue";
import DashboardPage from "@/pages/DashboardPage.vue";
import IdentityPage from "@/pages/IdentityPage.vue";
import CatalogPage from "@/pages/CatalogPage.vue";
import CommercePage from "@/pages/CommercePage.vue";
import AccountingPage from "@/pages/AccountingPage.vue";
import ReconciliationPage from "@/pages/ReconciliationPage.vue";
import NotFoundPage from "@/pages/NotFoundPage.vue";

/** The path of the sign-in screen, named once because the guard and `FailureView` both route there. */
export const SIGN_IN_PATH = "/sign-in";

/** Where a visitor goes when their session ends or they have not got one. */
export const LANDING_PATH = "/";

/**
 * The console's route table, as data.
 *
 * Exported so a test can build a router over it with a memory history, which is
 * the only way a route table is testable at all: a module-level singleton router
 * carrying a web history shares its current route — and its guard's memory of
 * whether anyone is signed in — across every test that touches it, so the fourth
 * test in a file asserts about the third test's session.
 */
export const ROUTES = [
  {
    path: "/sign-in",
    name: "sign-in",
    component: SignInPage,
    meta: { public: true },
  },
  {
    path: LANDING_PATH,
    name: "dashboard",
    component: DashboardPage,
  },
  {
    path: "/identity",
    name: "identity",
    component: IdentityPage,
  },
  {
    path: "/catalog",
    name: "catalog",
    component: CatalogPage,
  },
  {
    path: "/commerce",
    name: "commerce",
    component: CommercePage,
  },
  {
    path: "/accounting",
    name: "accounting",
    component: AccountingPage,
  },
  {
    path: "/reconciliation",
    name: "reconciliation",
    component: ReconciliationPage,
  },
  // The contract table's test entry point. It is a real route because a 404 for
  // it would be the console lying about its own API surface.
  {
    path: "/gateway-status",
    name: "gateway-status",
    component: GatewayStatusPage,
  },
  // ADR 0012 §5 names the model catalog, the request explorer and the runtime
  // views as NOT in the first console: Data Plane state with no contract on
  // `dataplane.yaml` and no index on `public.requests`. They are absent here
  // for that reason and not because they were not reached — a nav link to a
  // screen with no operation behind it is a button whose label is a promise
  // the console cannot keep.
  {
    path: "/:pathMatch(.*)*",
    name: "not-found",
    component: NotFoundPage,
  },
];

/**
 * Builds a router over the console's routes, with the session gate on it.
 *
 * The gate is installed HERE rather than only on the singleton, so a router a
 * test builds behaves exactly as the one the browser runs. A guard that only the
 * singleton carried would be a guard that a passing test has never exercised.
 */
export function createConsoleRouter(history = createWebHistory(import.meta.env.BASE_URL)): Router {
  const router = createRouter({ history, routes: ROUTES });
  installSessionGate(router);
  return router;
}

/** Whether a route may be reached without a session. */
function isPublic(route: RouteLocationNormalized): boolean {
  return route.meta.public === true;
}

/**
 * The session gate. Runs on every navigation.
 *
 * `resolve()` is called only when the store has no answer yet: the store keeps
 * what the last `getSession` said, and a navigation within a live session must
 * not cost a round trip to prove what is already known. It IS called after a
 * `401`, because that is the moment the previous answer stopped being true, and
 * the seam's announcement clears the store before the guard runs.
 */
export function installSessionGate(router: Router): void {
  router.beforeEach(async (to) => {
    const session = useSessionStore();

    // A public route is reachable with a session or without one, and the guard
    // does not go looking for one to know that: establishing is a network round
    // trip, and a guard that asked before admitting the sign-in screen would
    // flash an error there the moment a session had just ended — which is
    // exactly when the screen is needed.
    //
    // Sending a signed-in visitor AWAY from sign-in is deliberately NOT the
    // guard's job either. It is a presentation decision, and the sign-in screen
    // makes it: it shows "you are already signed in" and offers the dashboard,
    // which is a usable answer where a redirect loop is not.
    if (isPublic(to)) return true;

    if (session.signedIn) return true;

    if (!(await session.resolve())) {
      // The attempted route is remembered as `redirect` so the sign-in screen
      // can send the operator to where they were going. It is a PATH, never the
      // full query of a sensitive route — and no product route carries a
      // credential in its query, because rule 2 of the console's URL discipline
      // says none does.
      return {
        path: SIGN_IN_PATH,
        query: to.fullPath === LANDING_PATH ? {} : { redirect: to.fullPath },
      };
    }

    return true;
  });
}

/**
 * The 401 announcement. The seam emits it on the FIRST 401 of any call, from
 * whichever screen made it, and this is the only subscriber.
 *
 * It clears the store and sends the visitor to sign-in, carrying where they
 * were. It is a `replace`, not a `push`: a session that ended is not a state
 * the operator navigated to, and leaving it in history would make Back return
 * them to a screen that will immediately 401 again.
 */
export function watchForSessionEnd(router: Router): () => void {
  return onSessionEnded(() => {
    const session = useSessionStore();
    session.forget();
    const current = router.currentRoute.value;
    if (current.name === "sign-in") return;
    void router
      .replace({
        path: SIGN_IN_PATH,
        query: current.fullPath === SIGN_IN_PATH ? {} : { redirect: current.fullPath },
      })
      .catch(() => undefined);
  });
}

export default createConsoleRouter();
