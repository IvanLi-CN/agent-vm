import * as React from "react";
import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

const buttonVariants = cva(
  "inline-flex shrink-0 items-center justify-center gap-2 whitespace-nowrap rounded-[var(--radius-control)] border text-sm font-medium outline-none transition-colors focus-visible:ring-2 focus-visible:ring-[var(--accent)] focus-visible:ring-offset-2 focus-visible:ring-offset-[var(--surface)] disabled:pointer-events-none disabled:opacity-50 [&_svg]:pointer-events-none [&_svg]:size-4 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        default: "border-[var(--primary)] bg-[var(--primary)] text-white hover:bg-[#333638]",
        outline: "border-[var(--control)] bg-[var(--surface)] text-[var(--text)] hover:bg-[var(--raised)]",
        secondary: "border-[var(--border)] bg-[var(--raised)] text-[var(--text)] hover:bg-[var(--active)]",
        ghost: "border-transparent bg-transparent text-[var(--muted)] hover:bg-[var(--raised)] hover:text-[var(--text)]",
        destructive: "border-[#d9a9aa] bg-[var(--surface)] text-[var(--negative-ink)] hover:bg-[var(--negative-bg)]",
        link: "border-transparent bg-transparent p-0 text-[var(--accent)] underline-offset-4 hover:underline",
      },
      size: {
        default: "h-8 px-3",
        sm: "h-7 px-3 text-xs",
        lg: "h-9 px-4",
        icon: "size-8",
        "icon-sm": "size-7",
        "icon-lg": "size-9",
      },
    },
    defaultVariants: { variant: "default", size: "default" },
  },
);

export interface ButtonProps
  extends React.ButtonHTMLAttributes<HTMLButtonElement>,
    VariantProps<typeof buttonVariants> {
  asChild?: boolean;
}

const Button = React.forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { className, variant, size, asChild = false, ...props },
  ref,
) {
  const Comp = asChild ? Slot : "button";
  return <Comp ref={ref} data-slot="button" className={cn(buttonVariants({ variant, size, className }))} {...props} />;
});

Button.displayName = "Button";

export { Button, buttonVariants };
