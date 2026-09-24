import Link from "next/link";
import { ArrowLeft } from "lucide-react";
import { cn } from "@/lib/utils";

// Standard title/description/actions header used at the top of app pages.
// Detail screens pass backHref/backLabel for the breadcrumb-style back link;
// titleAccessory renders a chip (e.g. StatusChip) beside the h1.
export function PageHeader({
  title,
  description,
  actions,
  backHref,
  backLabel,
  titleAccessory,
  className,
}: {
  title: React.ReactNode;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  backHref?: string;
  backLabel?: React.ReactNode;
  titleAccessory?: React.ReactNode;
  className?: string;
}) {
  const heading = (
    <h1 className="text-xl font-semibold leading-tight tracking-tight text-foreground">
      {title}
    </h1>
  );

  return (
    <div
      className={cn("flex flex-wrap items-start justify-between gap-3", className)}
    >
      {/* `sm:flex-1` is what pins `actions` opposite the title rather than
          letting a long one push it onto a line of its own: while the title
          block is sized by its content, the wrapping happens before any
          shrinking can, so the width the title asks for is the width it gets.
          Below `sm` the wrap is still wanted — a narrow screen has no room to
          put both side by side. */}
      <div className="min-w-0 sm:flex-1">
        {backHref && (
          <Link
            href={backHref}
            className="mb-1.5 inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
          >
            <ArrowLeft className="size-4" /> {backLabel}
          </Link>
        )}
        {titleAccessory ? (
          <div className="flex items-center gap-2.5">
            {heading}
            {titleAccessory}
          </div>
        ) : (
          heading
        )}
        {description && (
          <p className="mt-0.5 text-sm text-muted-foreground">{description}</p>
        )}
      </div>
      {actions && (
        <div className="flex shrink-0 items-center gap-2">{actions}</div>
      )}
    </div>
  );
}
