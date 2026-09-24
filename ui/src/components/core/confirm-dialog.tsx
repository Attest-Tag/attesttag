"use client";

import * as React from "react";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { buttonVariants } from "@/components/ui/button";
import { cn } from "@/lib/utils";

type ConfirmOptions = {
  title?: string;
  description?: React.ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  /** Styles the confirm button as destructive (red). */
  destructive?: boolean;
};

type ConfirmState = ConfirmOptions & {
  open: boolean;
  resolve: ((ok: boolean) => void) | null;
};

// Imperative confirm() backed by a shadcn AlertDialog. Returns a promise that
// resolves true/false, so existing async flows can keep their shape:
//
//   const { confirm, confirmDialog } = useConfirm();
//   if (!(await confirm({ description: "Delete 3 items?", destructive: true }))) return;
//   ...
//   return (<>{trigger}{confirmDialog}</>);
//
// Render `confirmDialog` once anywhere in the component's JSX.
export function useConfirm() {
  const [state, setState] = React.useState<ConfirmState>({
    open: false,
    resolve: null,
  });

  const confirm = React.useCallback(
    (options: ConfirmOptions = {}) =>
      new Promise<boolean>((resolve) => {
        setState({ ...options, open: true, resolve });
      }),
    [],
  );

  const settle = (ok: boolean) => {
    state.resolve?.(ok);
    setState((s) => ({ ...s, open: false, resolve: null }));
  };

  const confirmDialog = (
    <AlertDialog
      open={state.open}
      onOpenChange={(open) => {
        // Closing via Escape / overlay counts as a cancel.
        if (!open) settle(false);
      }}
    >
      {/* max-w-md, not the primitive's max-w-lg: a confirm is two short lines,
          and this matches the app's other confirm surfaces (the document and
          deadline dialogs). */}
      <AlertDialogContent className="sm:max-w-md">
        <AlertDialogHeader>
          {/* "Are you sure?" is the last resort, not the norm — a confirm
              should name the action in its title. Callers pass one. */}
          <AlertDialogTitle>{state.title ?? "Are you sure?"}</AlertDialogTitle>
          {state.description != null && (
            <AlertDialogDescription>
              {state.description}
            </AlertDialogDescription>
          )}
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel onClick={() => settle(false)}>
            {state.cancelLabel ?? "Cancel"}
          </AlertDialogCancel>
          {/* Destructive confirms are red outline + red label, not a red fill —
              the same low-chrome treatment the bulk-action bar uses, so the
              button that commits the delete looks like the one that offered it.
              The tinted border is what separates it from Cancel. */}
          <AlertDialogAction
            className={
              state.destructive
                ? cn(
                    buttonVariants({ variant: "outline" }),
                    "border-destructive/40 bg-card text-destructive hover:bg-destructive/10 hover:text-destructive",
                  )
                : undefined
            }
            onClick={() => settle(true)}
          >
            {state.confirmLabel ?? "Confirm"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );

  return { confirm, confirmDialog };
}
