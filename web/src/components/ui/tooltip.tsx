import * as TooltipPrimitive from "@radix-ui/react-tooltip";
import { type ComponentPropsWithoutRef, type ElementRef, forwardRef } from "react";
import { cn } from "@/lib/utils";

const TooltipProvider = TooltipPrimitive.Provider;
const Tooltip = TooltipPrimitive.Root;
const TooltipTrigger = TooltipPrimitive.Trigger;

const TooltipContent = forwardRef<ElementRef<typeof TooltipPrimitive.Content>, ComponentPropsWithoutRef<typeof TooltipPrimitive.Content>>(function TooltipContent({ className, sideOffset = 5, ...props }, ref) {
  return <TooltipPrimitive.Portal><TooltipPrimitive.Content ref={ref} data-slot="tooltip-content" sideOffset={sideOffset} className={cn("z-50 rounded px-2.5 py-1.5 text-xs text-white shadow-lg", "bg-[var(--text)]", className)} {...props} /></TooltipPrimitive.Portal>;
});

export { TooltipProvider, Tooltip, TooltipTrigger, TooltipContent };
