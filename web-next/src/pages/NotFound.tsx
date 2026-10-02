import { Link } from "react-router-dom";
import { Home } from "lucide-react";
import { Button } from "@/components/ui";

export default function NotFound() {
  return (
    <div className="flex min-h-[60vh] flex-col items-center justify-center text-center">
      <div className="text-[80px] font-bold leading-none text-gradient">404</div>
      <h2 className="mt-4 text-xl font-semibold text-fg">页面不存在</h2>
      <p className="mt-2 max-w-sm text-sm text-fg-subtle">
        你访问的页面可能已被移除、重命名或暂时不可用。
      </p>
      <Link to="/" className="mt-6">
        <Button variant="brand" icon={<Home size={15} />}>
          返回首页
        </Button>
      </Link>
    </div>
  );
}
